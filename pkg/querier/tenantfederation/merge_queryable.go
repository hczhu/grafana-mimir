// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/tenantfederation/merge_queryable.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package tenantfederation

import (
	"context"
	"sort"
	"strings"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/concurrency"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	"github.com/weaveworks/common/user"
	"golang.org/x/exp/slices"

	"github.com/grafana/dskit/tenant"

	"github.com/grafana/mimir/pkg/util/spanlogger"
)

// NewQueryable returns a queryable that iterates through all the tenant IDs
// that are part of the request and aggregates the results from each tenant's
// Querier by sending of subsequent requests.
// By setting bypassWithSingleQuerier to true the mergeQuerier gets bypassed
// and results for request with a single querier will not contain the
// "__tenant_id__" label. This allows a smoother transition, when enabling
// tenant federation in a cluster.
// The result contains a label "__tenant_id__" to identify the tenant ID that
// it originally resulted from.
// If the label "__tenant_id__" is already existing, its value is overwritten
// by the tenant ID and the previous value is exposed through a new label
// prefixed with "original_". This behaviour is not implemented recursively.
func NewQueryable(upstream storage.Queryable, byPassWithSingleQuerier bool, logger log.Logger) storage.Queryable {
	return NewMergeQueryable(defaultTenantLabel, tenantQuerierCallback(upstream), byPassWithSingleQuerier, logger)
}

func tenantQuerierCallback(queryable storage.Queryable) MergeQuerierCallback {
	return func(ctx context.Context, mint int64, maxt int64) ([]string, []storage.Querier, error) {
		tenantIDs, err := tenant.TenantIDs(ctx)
		if err != nil {
			return nil, nil, err
		}

		var queriers = make([]storage.Querier, len(tenantIDs))
		for pos, tenantID := range tenantIDs {
			q, err := queryable.Querier(
				user.InjectOrgID(ctx, tenantID),
				mint,
				maxt,
			)
			if err != nil {
				return nil, nil, err
			}
			queriers[pos] = q
		}

		return tenantIDs, queriers, nil
	}
}

// MergeQuerierCallback returns the underlying queriers and their IDs relevant
// for the query.
type MergeQuerierCallback func(ctx context.Context, mint int64, maxt int64) (ids []string, queriers []storage.Querier, err error)

// NewMergeQueryable returns a queryable that merges results from multiple
// underlying Queryables. The underlying queryables and its label values to be
// considered are returned by a MergeQuerierCallback.
// By setting bypassWithSingleQuerier to true the mergeQuerier gets bypassed
// and results for request with a single querier will not contain the id label.
// This allows a smoother transition, when enabling tenant federation in a
// cluster.
// Results contain a label `idLabelName` to identify the underlying queryable
// that it originally resulted from.
// If the label `idLabelName` is already existing, its value is overwritten and
// the previous value is exposed through a new label prefixed with "original_".
// This behaviour is not implemented recursively.
func NewMergeQueryable(idLabelName string, callback MergeQuerierCallback, byPassWithSingleQuerier bool, logger log.Logger) storage.Queryable {
	return &mergeQueryable{
		logger:                  logger,
		idLabelName:             idLabelName,
		callback:                callback,
		bypassWithSingleQuerier: byPassWithSingleQuerier,
	}
}

type mergeQueryable struct {
	logger                  log.Logger
	idLabelName             string
	bypassWithSingleQuerier bool
	callback                MergeQuerierCallback
}

// Querier returns a new mergeQuerier, which aggregates results from multiple
// underlying queriers into a single result.
func (m *mergeQueryable) Querier(ctx context.Context, mint int64, maxt int64) (storage.Querier, error) {
	// TODO: it's necessary to think how to override context inside querier
	//  to mark spans created inside querier as child of a span created inside
	//  methods of merged querier.
	ids, queriers, err := m.callback(ctx, mint, maxt)
	if err != nil {
		return nil, err
	}

	// by pass when only single querier is returned
	if m.bypassWithSingleQuerier && len(queriers) == 1 {
		return queriers[0], nil
	}

	return &mergeQuerier{
		logger:      m.logger,
		ctx:         ctx,
		idLabelName: m.idLabelName,
		queriers:    queriers,
		ids:         ids,
	}, nil
}

// mergeQuerier aggregates the results from underlying queriers and adds a
// label `idLabelName` to identify the queryable that the metric resulted
// from.
// If the label `idLabelName` is already existing, its value is overwritten and
// the previous value is exposed through a new label prefixed with "original_".
// This behaviour is not implemented recursively
type mergeQuerier struct {
	logger      log.Logger
	ctx         context.Context
	queriers    []storage.Querier
	idLabelName string
	ids         []string
}

// LabelValues returns all potential values for a label name.  It is not safe
// to use the strings beyond the lifefime of the querier.
// For the label `idLabelName` it will return all the underlying ids available.
// For the label "original_" + `idLabelName it will return all the values
// of the underlying queriers for `idLabelName`.
func (m *mergeQuerier) LabelValues(name string, matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	spanlog, _ := spanlogger.NewWithLogger(m.ctx, m.logger, "mergeQuerier.LabelValues")
	defer spanlog.Finish()

	matchedTenants, filteredMatchers := filterValuesByMatchers(m.idLabelName, m.ids, matchers...)

	if name == m.idLabelName {
		var labelValues = make([]string, 0, len(matchedTenants))
		for _, id := range m.ids {
			if _, matched := matchedTenants[id]; matched {
				labelValues = append(labelValues, id)
			}
		}
		return labelValues, nil, nil
	}

	// ensure the name of a retained label gets handled under the original
	// label name
	if name == retainExistingPrefix+m.idLabelName {
		name = m.idLabelName
	}

	return m.mergeDistinctStringSliceWithTenants(func(ctx context.Context, q storage.Querier) ([]string, storage.Warnings, error) {
		return q.LabelValues(name, filteredMatchers...)
	}, matchedTenants)
}

// LabelNames returns all the unique label names present in the underlying
// queriers. It also adds the `idLabelName` and if present in the original
// results the original `idLabelName`.
func (m *mergeQuerier) LabelNames(matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	spanlog, _ := spanlogger.NewWithLogger(m.ctx, m.logger, "mergeQuerier.LabelNames")
	defer spanlog.Finish()

	matchedTenants, filteredMatchers := filterValuesByMatchers(m.idLabelName, m.ids, matchers...)

	labelNames, warnings, err := m.mergeDistinctStringSliceWithTenants(func(ctx context.Context, q storage.Querier) ([]string, storage.Warnings, error) {
		return q.LabelNames(filteredMatchers...)
	}, matchedTenants)
	if err != nil {
		return nil, nil, err
	}

	// check if the `idLabelName` exists in the original result
	var idLabelNameExists bool
	labelPos := sort.SearchStrings(labelNames, m.idLabelName)
	if labelPos < len(labelNames) && labelNames[labelPos] == m.idLabelName {
		idLabelNameExists = true
	}

	labelToAdd := m.idLabelName

	// if `idLabelName` already exists, we need to add the name prefix with
	// retainExistingPrefix.
	if idLabelNameExists {
		labelToAdd = retainExistingPrefix + m.idLabelName
		labelPos = sort.SearchStrings(labelNames, labelToAdd)
	}

	// insert label at the correct position
	labelNames = append(labelNames, "")
	copy(labelNames[labelPos+1:], labelNames[labelPos:])
	labelNames[labelPos] = labelToAdd

	return labelNames, warnings, nil
}

type stringSliceFunc func(context.Context, storage.Querier) ([]string, storage.Warnings, error)

type stringSliceFuncJob struct {
	querier  storage.Querier
	id       string
	result   []string
	warnings storage.Warnings
}

// mergeDistinctStringSliceWithTenants aggregates stringSliceFunc call
// results from queriers whose tenant ids match the tenants map. If a nil map is
// provided, all queriers are used. It removes duplicates and sorts the result.
// It doesn't require the output of the stringSliceFunc to be sorted, as results
// of LabelValues are not sorted.
func (m *mergeQuerier) mergeDistinctStringSliceWithTenants(f stringSliceFunc, tenants map[string]struct{}) ([]string, storage.Warnings, error) {
	jobs := make([]*stringSliceFuncJob, 0, len(m.ids))
	for pos, id := range m.ids {
		if tenants != nil {
			if _, matched := tenants[id]; !matched {
				continue
			}
		}

		jobs = append(jobs, &stringSliceFuncJob{
			querier: m.queriers[pos],
			id:      m.ids[pos],
		})
	}

	run := func(ctx context.Context, idx int) (err error) {
		job := jobs[idx]
		job.result, job.warnings, err = f(ctx, job.querier)
		if err != nil {
			return errors.Wrapf(err, "error querying %s %s", rewriteLabelName(m.idLabelName), job.id)
		}

		return nil
	}

	err := concurrency.ForEachJob(m.ctx, len(jobs), maxConcurrency, run)
	if err != nil {
		return nil, nil, err
	}

	// aggregate warnings and deduplicate string results
	var warnings storage.Warnings
	resultMap := make(map[string]struct{})
	for _, job := range jobs {
		for _, e := range job.result {
			resultMap[e] = struct{}{}
		}

		for _, w := range job.warnings {
			warnings = append(warnings, errors.Wrapf(w, "warning querying %s %s", rewriteLabelName(m.idLabelName), job.id))
		}
	}

	var result = make([]string, 0, len(resultMap))
	for e := range resultMap {
		result = append(result, e)
	}
	slices.Sort(result)
	return result, warnings, nil
}

// Close releases the resources of the Querier.
func (m *mergeQuerier) Close() error {
	errs := tsdb_errors.NewMulti()
	for pos, id := range m.ids {
		errs.Add(errors.Wrapf(m.queriers[pos].Close(), "failed to close querier for %s %s", rewriteLabelName(m.idLabelName), id))
	}
	return errs.Err()
}

type selectJob struct {
	querier storage.Querier
	id      string
}

// Select returns a set of series that matches the given label matchers. If the
// `idLabelName` is matched on, it only considers those queriers
// matching. The forwarded labelSelector is not containing those that operate
// on `idLabelName`.
func (m *mergeQuerier) Select(sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	spanlog, ctx := spanlogger.NewWithLogger(m.ctx, m.logger, "mergeQuerier.Select")
	defer spanlog.Finish()

	matchedValues, filteredMatchers := filterValuesByMatchers(m.idLabelName, m.ids, matchers...)

	var jobs = make([]*selectJob, len(matchedValues))
	var seriesSets = make([]storage.SeriesSet, len(matchedValues))
	var jobPos int
	for labelPos := range m.ids {
		if _, matched := matchedValues[m.ids[labelPos]]; !matched {
			continue
		}
		jobs[jobPos] = &selectJob{
			querier: m.queriers[labelPos],
			id:      m.ids[labelPos],
		}
		jobPos++
	}

	run := func(ctx context.Context, idx int) error {
		job := jobs[idx]
		seriesSets[idx] = &addLabelsSeriesSet{
			upstream: job.querier.Select(sortSeries, hints, filteredMatchers...),
			labels: []labels.Label{
				{
					Name:  m.idLabelName,
					Value: job.id,
				},
			},
		}
		return nil
	}

	err := concurrency.ForEachJob(ctx, len(jobs), maxConcurrency, run)
	if err != nil {
		return storage.ErrSeriesSet(err)
	}

	return storage.NewMergeSeriesSet(seriesSets, storage.ChainedSeriesMerge)
}

type addLabelsSeriesSet struct {
	upstream   storage.SeriesSet
	labels     []labels.Label
	currSeries storage.Series
}

func (m *addLabelsSeriesSet) Next() bool {
	m.currSeries = nil
	return m.upstream.Next()
}

// At returns full series. Returned series should be iteratable even after Next is called.
func (m *addLabelsSeriesSet) At() storage.Series {
	if m.currSeries == nil {
		upstream := m.upstream.At()
		m.currSeries = &addLabelsSeries{
			upstream: upstream,
			labels:   setLabelsRetainExisting(upstream.Labels(), m.labels...),
		}
	}
	return m.currSeries
}

// The error that iteration as failed with.
// When an error occurs, set cannot continue to iterate.
func (m *addLabelsSeriesSet) Err() error {
	return errors.Wrapf(m.upstream.Err(), "error querying %s", labelsToString(m.labels))
}

// A collection of warnings for the whole set.
// Warnings could be return even iteration has not failed with error.
func (m *addLabelsSeriesSet) Warnings() storage.Warnings {
	upstream := m.upstream.Warnings()
	warnings := make(storage.Warnings, len(upstream))
	for pos := range upstream {
		warnings[pos] = errors.Wrapf(upstream[pos], "warning querying %s", labelsToString(m.labels))
	}
	return warnings
}

// rewrite label name to be more readable in error output
func rewriteLabelName(s string) string {
	return strings.TrimRight(strings.TrimLeft(s, "_"), "_")
}

// this outputs a more readable error format
func labelsToString(labels []labels.Label) string {
	parts := make([]string, len(labels))
	for pos, l := range labels {
		parts[pos] = rewriteLabelName(l.Name) + " " + l.Value
	}
	return strings.Join(parts, ", ")
}

type addLabelsSeries struct {
	upstream storage.Series
	labels   labels.Labels
}

// Labels returns the complete set of labels. For series it means all labels identifying the series.
func (a *addLabelsSeries) Labels() labels.Labels {
	return a.labels
}

// Iterator returns a new, independent iterator of the data of the series.
func (a *addLabelsSeries) Iterator(i chunkenc.Iterator) chunkenc.Iterator {
	return a.upstream.Iterator(i)
}
