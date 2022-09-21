// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package binary

import (
	"context"
	"sync"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/thanos-community/promql-engine/physicalplan/model"
)

// vectorOperator evaluates an expression between two step vectors.
type vectorOperator struct {
	pool *model.VectorPool
	once sync.Once

	lhs       model.VectorOperator
	rhs       model.VectorOperator
	matching  *parser.VectorMatching
	operation parser.ItemType

	// series contains the output series of the operator
	series []labels.Labels
	// The outputCache is an internal cache used to calculate
	// the binary operation of the lhs and rhs operator.
	outputCache []sample
	// highCardOutputIndex is a mapping from series ID of the high cardinality
	// operator to an output series ID.
	// The value is nullable since during joins, certain lhs series can fail to
	// find a matching rhs series.
	highCardOutputIndex []*uint64
	// lowCardOutputIndex is a mapping from series ID of the low cardinality
	// operator to an output series ID.
	// Each series from the low cardinality operator can join with many
	// series of the high cardinality operator.
	lowCardOutputIndex [][]uint64
	// table is used to calculate the binary operation of two step vectors between
	// the lhs and rhs operator.
	table *table
}

func NewVectorOperator(
	pool *model.VectorPool,
	lhs model.VectorOperator,
	rhs model.VectorOperator,
	matching *parser.VectorMatching,
	operation parser.ItemType,
) (model.VectorOperator, error) {
	return &vectorOperator{
		pool:      pool,
		lhs:       lhs,
		rhs:       rhs,
		matching:  matching,
		operation: operation,
	}, nil
}

func (o *vectorOperator) Series(ctx context.Context) ([]labels.Labels, error) {
	var err error
	o.once.Do(func() { err = o.initOutputs(ctx) })
	if err != nil {
		return nil, err
	}

	return o.series, nil
}

func (o *vectorOperator) initOutputs(ctx context.Context) error {
	// TODO(fpetkovski): execute in parallel
	highCardSide, err := o.lhs.Series(ctx)
	if err != nil {
		return err
	}
	lowCardSide, err := o.rhs.Series(ctx)
	if err != nil {
		return err
	}
	if o.matching.Card == parser.CardOneToMany {
		highCardSide, lowCardSide = lowCardSide, highCardSide
	}

	buf := make([]byte, 128)
	highCardHashes, highCardInputMap := o.hashSeries(highCardSide, true, buf)
	lowCardHashes, lowCardInputMap := o.hashSeries(lowCardSide, false, buf)
	output, highCardOutputIndex, lowCardOutputIndex := o.join(highCardHashes, highCardInputMap, lowCardHashes, lowCardInputMap)

	series := make([]labels.Labels, len(output))
	for _, s := range output {
		series[s.ID] = s.Metric
	}
	o.series = series
	o.highCardOutputIndex = highCardOutputIndex
	o.lowCardOutputIndex = lowCardOutputIndex
	o.outputCache = make([]sample, len(series))
	o.pool.SetStepSize(len(highCardSide))

	t, err := newTable(o.pool, o.matching.Card, o.operation, o.outputCache, highCardOutputIndex, lowCardOutputIndex)
	if err != nil {
		return err
	}
	o.table = t

	return nil
}

func (o *vectorOperator) Next(ctx context.Context) ([]model.StepVector, error) {
	lhs, err := o.lhs.Next(ctx)
	if err != nil {
		return nil, err
	}
	rhs, err := o.rhs.Next(ctx)
	if err != nil {
		return nil, err
	}

	// TODO(fpetkovski): When one operator becomes empty,
	// we might want to drain or close the other one.
	// We don't have a concept of closing an operator yet.
	if len(lhs) == 0 || len(rhs) == 0 {
		return nil, nil
	}

	o.once.Do(func() { err = o.initOutputs(ctx) })
	if err != nil {
		return nil, err
	}

	batch := o.pool.GetVectorBatch()
	for i, vector := range lhs {
		step := o.table.execBinaryOperation(lhs[i], rhs[i])
		batch = append(batch, step)
		o.lhs.GetPool().PutStepVector(vector)
		if i < len(rhs) {
			o.rhs.GetPool().PutStepVector(rhs[i])
		}
	}
	o.lhs.GetPool().PutVectors(lhs)
	o.rhs.GetPool().PutVectors(rhs)

	return batch, nil
}

func (o *vectorOperator) GetPool() *model.VectorPool {
	return o.pool
}

// hashSeries calculates the hash of each series from an input operator.
// Since series from the high cardinality operator can map to multiple output series,
// hashSeries returns an index from hash to a slice of resulting series, and
// a map from input series ID to output series ID.
// The latter can be used to build an array backed index from input model.Series to output model.Series,
// avoiding expensive hashmap lookups.
func (o *vectorOperator) hashSeries(series []labels.Labels, keepLabels bool, buf []byte) (map[uint64][]model.Series, map[uint64][]uint64) {
	hashes := make(map[uint64][]model.Series)
	inputIndex := make(map[uint64][]uint64)
	for i, s := range series {
		sig, lbls := signature(s, !o.matching.On, o.matching.MatchingLabels, keepLabels, buf)
		if _, ok := hashes[sig]; !ok {
			hashes[sig] = make([]model.Series, 0, 1)
			inputIndex[sig] = make([]uint64, 0, 1)
		}
		hashes[sig] = append(hashes[sig], model.Series{
			ID:     uint64(i),
			Metric: lbls,
		})
		inputIndex[sig] = append(inputIndex[sig], uint64(i))
	}

	return hashes, inputIndex
}

// join performs a join between series from the high cardinality and low cardinality operators.
// It does that by using hash maps which point from series hash to the output series.
// It also returns array backed indices for the high cardinality and low cardinality operators,
// pointing from input model.Series ID to output model.Series ID.
// The high cardinality operator can fail to join, which is why its index contains nullable values.
// The low cardinality operator can join to multiple high cardinality series, which is why its index
// points to an array of output series.
func (o *vectorOperator) join(
	highCardHashes map[uint64][]model.Series,
	highCardInputIndex map[uint64][]uint64,
	lowCardHashes map[uint64][]model.Series,
	lowCardInputIndex map[uint64][]uint64,
) ([]model.Series, []*uint64, [][]uint64) {
	// Output index points from output series ID
	// to the actual series.
	outputIndex := make([]model.Series, 0)

	// Prune high cardinality series which do not have a
	// matching low cardinality series.
	outputSize := 0
	for hash, series := range highCardHashes {
		outputSize += len(series)
		if _, ok := lowCardHashes[hash]; !ok {
			delete(highCardHashes, hash)
			continue
		}
	}

	highCardOutputIndex := make([]*uint64, outputSize)
	lowCardOutputIndex := make([][]uint64, outputSize)
	for hash, outputSeries := range highCardHashes {
		lowCardSeriesID := lowCardInputIndex[hash][0]
		// Each low cardinality series can map to multiple output series.
		lowCardOutputIndex[lowCardSeriesID] = make([]uint64, 0, len(outputSeries))

		for i, output := range outputSeries {
			outputSeries := model.Series{ID: uint64(len(outputIndex)), Metric: output.Metric}
			outputIndex = append(outputIndex, outputSeries)

			highCardSeriesID := highCardInputIndex[hash][i]
			highCardOutputIndex[highCardSeriesID] = &outputSeries.ID
			lowCardOutputIndex[lowCardSeriesID] = append(lowCardOutputIndex[lowCardSeriesID], outputSeries.ID)
		}
	}

	return outputIndex, highCardOutputIndex, lowCardOutputIndex
}

func signature(metric labels.Labels, without bool, grouping []string, keepLabels bool, buf []byte) (uint64, labels.Labels) {
	buf = buf[:0]
	lb := labels.NewBuilder(metric).Del(labels.MetricName)
	if without {
		dropLabels := append(grouping, labels.MetricName)
		key, _ := metric.HashWithoutLabels(buf, dropLabels...)
		if !keepLabels {
			lb.Del(dropLabels...)
		}
		return key, lb.Labels()
	}

	if keepLabels {
		lb.Keep(grouping...)
	}
	if len(grouping) == 0 {
		return 0, lb.Labels()
	}

	key, _ := metric.HashForLabels(buf, grouping...)
	return key, lb.Labels()
}
