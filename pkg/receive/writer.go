// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package receive

import (
	"context"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"

	"github.com/thanos-io/thanos/pkg/errutil"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/store/storepb/prompb"
)

// Appendable returns an Appender.
type Appendable interface {
	Appender(ctx context.Context) (storage.Appender, error)
}

type TenantStorage interface {
	TenantAppendable(string) (Appendable, error)
}

type Writer struct {
	logger    log.Logger
	multiTSDB TenantStorage
}

func NewWriter(logger log.Logger, multiTSDB TenantStorage) *Writer {
	return &Writer{
		logger:    logger,
		multiTSDB: multiTSDB,
	}
}

func (r *Writer) Write(ctx context.Context, tenantID string, wreq *prompb.WriteRequest) error {
	tLogger := log.With(r.logger, "tenant", tenantID)

	var (
		numLabelsOutOfOrder = 0
		numLabelsDuplicates = 0
		numLabelsEmpty      = 0

		numSamplesOutOfOrder  = 0
		numSamplesDuplicates  = 0
		numSamplesOutOfBounds = 0

		numExemplarsOutOfOrder  = 0
		numExemplarsDuplicate   = 0
		numExemplarsLabelLength = 0
	)

	s, err := r.multiTSDB.TenantAppendable(tenantID)
	if err != nil {
		return errors.Wrap(err, "get tenant appendable")
	}

	app, err := s.Appender(ctx)
	if err == tsdb.ErrNotReady {
		return err
	}
	if err != nil {
		return errors.Wrap(err, "get appender")
	}
	getRef := app.(storage.GetRef)

	var (
		ref  storage.SeriesRef
		errs errutil.MultiError
	)
	for _, t := range wreq.Timeseries {
		// Check if time series labels are valid. If not, skip the time series
		// and report the error.
		if err := labelpb.ValidateLabels(t.Labels); err != nil {
			lset := &labelpb.ZLabelSet{Labels: t.Labels}
			switch err {
			case labelpb.ErrOutOfOrderLabels:
				numLabelsOutOfOrder++
				level.Debug(tLogger).Log("msg", "Out of order labels in the label set", "lset", lset.String())
			case labelpb.ErrDuplicateLabels:
				numLabelsDuplicates++
				level.Debug(tLogger).Log("msg", "Duplicate labels in the label set", "lset", lset.String())
			case labelpb.ErrEmptyLabels:
				numLabelsEmpty++
				level.Debug(tLogger).Log("msg", "Labels with empty name in the label set", "lset", lset.String())
			default:
				level.Debug(tLogger).Log("msg", "Error validating labels", "err", err)
			}

			continue
		}

		lset := labelpb.ZLabelsToPromLabels(t.Labels)

		// Check if the TSDB has cached reference for those labels.
		ref, lset = getRef.GetRef(lset)
		if ref == 0 {
			// If not, copy labels, as TSDB will hold those strings long term. Given no
			// copy unmarshal we don't want to keep memory for whole protobuf, only for labels.
			labelpb.ReAllocZLabelsStrings(&t.Labels)
			lset = labelpb.ZLabelsToPromLabels(t.Labels)
		}

		// Append as many valid samples as possible, but keep track of the errors.
		for _, s := range t.Samples {
			ref, err = app.Append(ref, lset, s.Timestamp, s.Value)
			switch err {
			case storage.ErrOutOfOrderSample:
				numSamplesOutOfOrder++
				level.Debug(tLogger).Log("msg", "Out of order sample", "lset", lset, "value", s.Value, "timestamp", s.Timestamp)
			case storage.ErrDuplicateSampleForTimestamp:
				numSamplesDuplicates++
				level.Debug(tLogger).Log("msg", "Duplicate sample for timestamp", "lset", lset, "value", s.Value, "timestamp", s.Timestamp)
			case storage.ErrOutOfBounds:
				numSamplesOutOfBounds++
				level.Debug(tLogger).Log("msg", "Out of bounds metric", "lset", lset, "value", s.Value, "timestamp", s.Timestamp)
			default:
				if err != nil {
					level.Debug(tLogger).Log("msg", "Error ingesting sample", "err", err)
				}
			}
		}

		// Current implemetation of app.AppendExemplar doesn't create a new series, so it must be already present.
		// We drop the exemplars in case the series doesn't exist.
		if ref != 0 && len(t.Exemplars) > 0 {
			for _, ex := range t.Exemplars {
				exLset := labelpb.ZLabelsToPromLabels(ex.Labels)
				exLogger := log.With(tLogger, "exemplarLset", exLset, "exemplar", ex.String())

				if _, err = app.AppendExemplar(ref, lset, exemplar.Exemplar{
					Labels: exLset,
					Value:  ex.Value,
					Ts:     ex.Timestamp,
					HasTs:  true,
				}); err != nil {
					switch err {
					case storage.ErrOutOfOrderExemplar:
						numExemplarsOutOfOrder++
						level.Debug(exLogger).Log("msg", "Out of order exemplar")
					case storage.ErrDuplicateExemplar:
						numExemplarsDuplicate++
						level.Debug(exLogger).Log("msg", "Duplicate exemplar")
					case storage.ErrExemplarLabelLength:
						numExemplarsLabelLength++
						level.Debug(exLogger).Log("msg", "Label length for exemplar exceeds max limit", "limit", exemplar.ExemplarMaxLabelSetLength)
					default:
						if err != nil {
							level.Debug(exLogger).Log("msg", "Error ingesting exemplar", "err", err)
						}
					}
				}
			}
		}
	}

	if numLabelsOutOfOrder > 0 {
		level.Warn(tLogger).Log("msg", "Error on series with out-of-order labels", "numDropped", numLabelsOutOfOrder)
		errs.Add(errors.Wrapf(labelpb.ErrOutOfOrderLabels, "add %d series", numLabelsOutOfOrder))
	}
	if numLabelsDuplicates > 0 {
		level.Warn(tLogger).Log("msg", "Error on series with duplicate labels", "numDropped", numLabelsDuplicates)
		errs.Add(errors.Wrapf(labelpb.ErrDuplicateLabels, "add %d series", numLabelsDuplicates))
	}
	if numLabelsEmpty > 0 {
		level.Warn(tLogger).Log("msg", "Error on series with empty label name or value", "numDropped", numLabelsEmpty)
		errs.Add(errors.Wrapf(labelpb.ErrEmptyLabels, "add %d series", numLabelsEmpty))
	}

	if numSamplesOutOfOrder > 0 {
		level.Warn(tLogger).Log("msg", "Error on ingesting out-of-order samples", "numDropped", numSamplesOutOfOrder)
		errs.Add(errors.Wrapf(storage.ErrOutOfOrderSample, "add %d samples", numSamplesOutOfOrder))
	}
	if numSamplesDuplicates > 0 {
		level.Warn(tLogger).Log("msg", "Error on ingesting samples with different value but same timestamp", "numDropped", numSamplesDuplicates)
		errs.Add(errors.Wrapf(storage.ErrDuplicateSampleForTimestamp, "add %d samples", numSamplesDuplicates))
	}
	if numSamplesOutOfBounds > 0 {
		level.Warn(tLogger).Log("msg", "Error on ingesting samples that are too old or are too far into the future", "numDropped", numSamplesOutOfBounds)
		errs.Add(errors.Wrapf(storage.ErrOutOfBounds, "add %d samples", numSamplesOutOfBounds))
	}

	if numExemplarsOutOfOrder > 0 {
		level.Warn(tLogger).Log("msg", "Error on ingesting out-of-order exemplars", "numDropped", numExemplarsOutOfOrder)
		errs.Add(errors.Wrapf(storage.ErrOutOfOrderExemplar, "add %d exemplars", numExemplarsOutOfOrder))
	}
	if numExemplarsDuplicate > 0 {
		level.Warn(tLogger).Log("msg", "Error on ingesting duplicate exemplars", "numDropped", numExemplarsDuplicate)
		errs.Add(errors.Wrapf(storage.ErrDuplicateExemplar, "add %d exemplars", numExemplarsDuplicate))
	}
	if numExemplarsLabelLength > 0 {
		level.Warn(tLogger).Log("msg", "Error on ingesting exemplars with label length exceeding maximum limit", "numDropped", numExemplarsLabelLength)
		errs.Add(errors.Wrapf(storage.ErrExemplarLabelLength, "add %d exemplars", numExemplarsLabelLength))
	}

	if err := app.Commit(); err != nil {
		errs.Add(errors.Wrap(err, "commit samples"))
	}
	return errs.Err()
}
