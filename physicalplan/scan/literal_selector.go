// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package scan

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/thanos-community/promql-engine/physicalplan/model"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
)

// numberLiteralSelector returns []model.StepVector with same sample value across time range.
type numberLiteralSelector struct {
	vectorPool *model.VectorPool

	mint        int64
	maxt        int64
	step        int64
	currentStep int64
	stepsBatch  int
	series      []labels.Labels
	once        sync.Once

	val  float64
	call FunctionCall
}

func NewNumberLiteralSelector(pool *model.VectorPool, mint, maxt time.Time, step time.Duration, stepsBatch int, val float64, call FunctionCall) model.VectorOperator {
	return &numberLiteralSelector{
		vectorPool:  pool,
		mint:        mint.UnixMilli(),
		maxt:        maxt.UnixMilli(),
		step:        step.Milliseconds(),
		currentStep: mint.UnixMilli(),
		stepsBatch:  stepsBatch,
		val:         val,
		call:        call,
	}
}

func (o *numberLiteralSelector) Series(ctx context.Context) ([]labels.Labels, error) {
	o.loadSeries()
	return o.series, nil
}

func (o *numberLiteralSelector) GetPool() *model.VectorPool {
	return o.vectorPool
}

func (o *numberLiteralSelector) Next(ctx context.Context) ([]model.StepVector, error) {
	if o.currentStep > o.maxt {
		return nil, nil
	}

	o.loadSeries()

	totalSteps := int64(1)
	if o.step != 0 {
		totalSteps = (o.maxt-o.mint)/o.step + 1
	}

	vectors := o.vectorPool.GetVectorBatch()
	numSteps := int(math.Min(float64(o.stepsBatch), float64(totalSteps)))
	ts := o.currentStep

	for currStep := 0; currStep < numSteps && ts <= o.maxt; currStep++ {
		if len(vectors) <= currStep {
			vectors = append(vectors, o.vectorPool.GetStepVector(ts))
		}

		result := promql.Sample{
			Point: promql.Point{
				T: ts,
				V: o.val,
			},
		}

		if o.call != nil {
			result = o.call(o.series[0], []promql.Point{result.Point}, time.UnixMilli(ts))
		}

		vectors[currStep].T = result.T
		vectors[currStep].SampleIDs = append(vectors[currStep].SampleIDs, uint64(0))
		vectors[currStep].Samples = append(vectors[currStep].Samples, result.V)

		ts += o.step
	}

	o.currentStep += o.step * int64(numSteps)

	return vectors, nil
}

func (o *numberLiteralSelector) loadSeries() {
	// If number literal is included within function, []labels.labels must be initialized.
	o.once.Do(func() {
		o.series = make([]labels.Labels, 1)

		if o.call != nil {
			o.series = []labels.Labels{labels.New()}
		}
	})
}
