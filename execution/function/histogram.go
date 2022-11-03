// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package function

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/thanos-community/promql-engine/execution/model"
)

type histogramSeries struct {
	outputID   int
	upperBound float64
}

// histogramOperator is a function operator that calculates percentiles.
type histogramOperator struct {
	pool *model.VectorPool

	funcArgs parser.Expressions

	once     sync.Once
	series   []labels.Labels
	scalarOp model.VectorOperator
	vectorOp model.VectorOperator

	// scalarPoints is a reusable buffer for points from the first argument of histogram_quantile.
	scalarPoints []float64
	// outputIndex is a mapping from input series ID to the output series ID and its upper boundary value
	// parsed from the le label.
	outputIndex []histogramSeries
	// seriesBuckets are the buckets for each individual series.
	seriesBuckets []buckets
}

func NewHistogramOperator(pool *model.VectorPool, args parser.Expressions, nextOps []model.VectorOperator, stepsBatch int) (model.VectorOperator, error) {
	return &histogramOperator{
		pool:         pool,
		funcArgs:     args,
		once:         sync.Once{},
		scalarOp:     nextOps[0],
		vectorOp:     nextOps[1],
		scalarPoints: make([]float64, stepsBatch),
	}, nil
}

func (o *histogramOperator) Explain() (me string, next []model.VectorOperator) {
	next = []model.VectorOperator{o.scalarOp, o.vectorOp}
	return fmt.Sprintf("[*functionOperator] histogram_quantile(%v)", o.funcArgs), next
}

func (o *histogramOperator) Series(ctx context.Context) ([]labels.Labels, error) {
	var err error
	o.once.Do(func() { err = o.loadSeries(ctx) })
	if err != nil {
		return nil, err
	}

	return o.series, nil
}

func (o *histogramOperator) GetPool() *model.VectorPool {
	return o.pool
}

func (o *histogramOperator) Next(ctx context.Context) ([]model.StepVector, error) {
	var err error
	o.once.Do(func() { err = o.loadSeries(ctx) })
	if err != nil {
		return nil, err
	}

	scalars, err := o.scalarOp.Next(ctx)
	if err != nil {
		return nil, err
	}

	if len(scalars) == 0 {
		return nil, nil
	}

	vectors, err := o.vectorOp.Next(ctx)
	if err != nil {
		return nil, err
	}

	o.scalarPoints = o.scalarPoints[:0]
	for _, scalar := range scalars {
		if len(scalar.Samples) > 0 {
			o.scalarPoints = append(o.scalarPoints, scalar.Samples[0])
		}
		o.scalarOp.GetPool().PutStepVector(scalar)
	}
	o.scalarOp.GetPool().PutVectors(scalars)

	return o.processInputSeries(vectors)
}

func (o *histogramOperator) processInputSeries(vectors []model.StepVector) ([]model.StepVector, error) {
	out := o.pool.GetVectorBatch()
	for stepIndex, vector := range vectors {
		o.resetBuckets()
		for i, seriesID := range vector.SampleIDs {
			outputSeries := o.outputIndex[seriesID]
			outputSeriesID := outputSeries.outputID
			bucket := le{
				upperBound: outputSeries.upperBound,
				count:      vector.Samples[i],
			}
			o.seriesBuckets[outputSeriesID] = append(o.seriesBuckets[outputSeriesID], bucket)
		}

		step := o.pool.GetStepVector(vector.T)
		for i, stepBuckets := range o.seriesBuckets {
			// We need at least2 buckets to calculate a quantile.
			if len(stepBuckets) < 2 {
				continue
			}

			if stepIndex >= len(o.scalarPoints) {
				step.SampleIDs = append(step.SampleIDs, uint64(i))
				step.Samples = append(step.Samples, math.NaN())
				continue
			}

			val := bucketQuantile(o.scalarPoints[stepIndex], stepBuckets)
			step.SampleIDs = append(step.SampleIDs, uint64(i))
			step.Samples = append(step.Samples, val)
		}
		out = append(out, step)
		o.scalarOp.GetPool().PutStepVector(vector)
	}

	o.scalarOp.GetPool().PutVectors(vectors)
	return out, nil
}

func (o *histogramOperator) loadSeries(ctx context.Context) error {
	series, err := o.vectorOp.Series(ctx)
	if err != nil {
		return err
	}

	var (
		hashBuf      = make([]byte, 0, 256)
		hasher       = xxhash.New()
		seriesHashes = make(map[uint64]int, len(series))
	)

	o.series = make([]labels.Labels, 0)
	o.outputIndex = make([]histogramSeries, len(series))

	for i, s := range series {
		lbls, bucketLabel := dropLabel(s, "le")
		value, err := strconv.ParseFloat(bucketLabel.Value, 64)
		if err != nil {
			continue
		}
		lbls, _ = DropMetricName(lbls)

		hasher.Reset()
		hashBuf = lbls.Bytes(hashBuf)
		if _, err := hasher.Write(hashBuf); err != nil {
			return err
		}

		seriesHash := hasher.Sum64()
		seriesID, ok := seriesHashes[seriesHash]
		if !ok {
			o.series = append(o.series, lbls)
			seriesID = len(o.series) - 1
			seriesHashes[seriesHash] = seriesID
		}

		o.outputIndex[i] = histogramSeries{
			outputID:   seriesID,
			upperBound: value,
		}
	}

	o.seriesBuckets = make([]buckets, len(o.series))
	o.pool.SetStepSize(len(o.series))
	return nil
}

func (o *histogramOperator) resetBuckets() {
	for i := range o.seriesBuckets {
		o.seriesBuckets[i] = o.seriesBuckets[i][:0]
	}
}