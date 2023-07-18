package logql

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/go-kit/log"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promql_parser "github.com/prometheus/prometheus/promql/parser"

	"github.com/grafana/dskit/tenant"

	"github.com/grafana/loki/pkg/logql/sketch"
	"github.com/grafana/loki/pkg/logql/syntax"
	"github.com/grafana/loki/pkg/logqlmodel"
	"github.com/grafana/loki/pkg/util"
	"github.com/grafana/loki/pkg/util/validation"
)

type T int64

const (
	VecType T = iota
	TopKVecType
)

type StepResult interface {
	Type() T

	SampleVector() promql.Vector
	TopkVector() []sketch.Topk
}

type SampleVector promql.Vector

func (SampleVector) Type() T {
	return VecType
}

func (s SampleVector) SampleVector() promql.Vector {
	return promql.Vector(s)
}

func (s SampleVector) TopkVector() []sketch.Topk {
	return nil
}

type TopKVector []sketch.Topk

func (TopKVector) Type() T {
	return TopKVecType
}

func (v TopKVector) SampleVector() promql.Vector {
	return nil
}

func (v TopKVector) TopkVector() []sketch.Topk {
	return v
}

// ProbabilisticStepEvaluator evaluate a single step of a probabilistic query type.
type ProbabilisticStepEvaluator interface {
	// while Next returns a promql.Value, the only acceptable types are Scalar and Vector.
	Next() (ok bool, ts int64, r StepResult)
	// Close all resources used.
	Close() error
	// Reports any error
	Error() error

	Type() T
}

type ProbabilisticEvaluator struct {
	DefaultEvaluator
	logger log.Logger
}

type pStepEvaluator struct {
	fn    func() (bool, int64, StepResult)
	close func() error
	err   func() error
	t     T
}

func (e *pStepEvaluator) Type() T {
	return e.t
}

func (e *pStepEvaluator) Next() (bool, int64, StepResult) {
	return e.fn()
}

func (e *pStepEvaluator) Close() error {
	return e.close()
}

func (e *pStepEvaluator) Error() error {
	return e.err()
}

// this function is just copy paste from the regular step eval, maybe we don't need it?
func NewProbabilisticStepEvaluator(fn func() (bool, int64, StepResult), closeFn func() error, err func() error) (ProbabilisticStepEvaluator, error) {
	if fn == nil {
		return nil, errors.New("nil step evaluator fn")
	}

	if closeFn == nil {
		closeFn = func() error { return nil }
	}

	if err == nil {
		err = func() error { return nil }
	}
	return &pStepEvaluator{
		fn:    fn,
		close: closeFn,
		err:   err,
	}, nil
}

// NewDefaultEvaluator constructs a DefaultEvaluator
func NewProbabilisticEvaluator(querier Querier, maxLookBackPeriod time.Duration) Evaluator {
	d := NewDefaultEvaluator(querier, maxLookBackPeriod)
	p := &ProbabilisticEvaluator{DefaultEvaluator: *d}
	return p
}

type testEval struct {
	d StepEvaluator
}

func (p testEval) Next() (ok bool, ts int64, r StepResult) {
	a, s, d := p.d.Next()
	return a, s, SampleVector(d)
}

func (p testEval) Close() error {
	return p.d.Close()
}

func (p testEval) Error() error {
	return p.d.Error()
}

func (p testEval) Type() T {
	return p.d.Type()
}

func (p *ProbabilisticEvaluator) ProbabilisticStepEvaluator(
	ctx context.Context,
	nextEv SampleEvaluator,
	expr syntax.SampleExpr,
	q Params,
) (ProbabilisticStepEvaluator, error) {

	switch e := expr.(type) {
	case *syntax.VectorAggregationExpr:
		if e.Operation != syntax.OpTypeTopK {
			dEval, err := p.DefaultEvaluator.StepEvaluator(ctx, nextEv, expr, q)
			if err != nil {
				return nil, err
			}
			return testEval{dEval}, nil
		}
		return p.newProbabilisticVectorAggEvaluator(ctx, nextEv, e, q)
	default:
		dEval, err := p.DefaultEvaluator.StepEvaluator(ctx, nextEv, expr, q)
		if err != nil {
			return nil, err
		}
		return testEval{dEval}, nil
	}
}

type probabilisticQuery struct {
	evaluator Evaluator
	query
}

// Exec Implements `Query`. It handles instrumentation & defers to Eval.
func (q *probabilisticQuery) Exec(ctx context.Context) (logqlmodel.Result, error) {
	// TODO(karsten): avoid copying all of Exec. We do so now so that we can
	// explore the proper interfaces.
	sp, ctx := opentracing.StartSpanFromContext(ctx, "query.Exec")
	defer sp.Finish()

	data, err := q.Eval(ctx)

	// TODO(karsten): record query statistics
	//statResult := statsCtx.Result(time.Since(start), queueTime, q.resultLength(data))
	//statResult.Log(level.Debug(spLogger))

	return logqlmodel.Result{
		Data: data,
	}, err
}

func (q *probabilisticQuery) Eval(ctx context.Context) (promql_parser.Value, error) {
	tenants, _ := tenant.TenantIDs(ctx)
	timeoutCapture := func(id string) time.Duration { return q.limits.QueryTimeout(ctx, id) }
	queryTimeout := validation.SmallestPositiveNonZeroDurationPerTenant(tenants, timeoutCapture)
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	expr, err := q.parse(ctx, q.params.Query())
	if err != nil {
		return nil, err
	}

	if q.checkBlocked(ctx, tenants) {
		return nil, logqlmodel.ErrBlocked
	}

	switch e := expr.(type) {
	case syntax.SampleExpr:
		value, err := q.probabilisticEvalSample(ctx, e)
		return value, err

	default:
		return q.query.Eval(ctx)
	}
}

// evalSample evaluate a sampleExpr
func (q *probabilisticQuery) probabilisticEvalSample(ctx context.Context, expr syntax.SampleExpr) (promql_parser.Value, error) {
	if lit, ok := expr.(*syntax.LiteralExpr); ok {
		return q.evalLiteral(ctx, lit)
	}
	if vec, ok := expr.(*syntax.VectorExpr); ok {
		return q.evalVector(ctx, vec)
	}

	tenantIDs, err := tenant.TenantIDs(ctx)
	if err != nil {
		return nil, err
	}

	maxIntervalCapture := func(id string) time.Duration { return q.limits.MaxQueryRange(ctx, id) }
	maxQueryInterval := validation.SmallestPositiveNonZeroDurationPerTenant(tenantIDs, maxIntervalCapture)
	if maxQueryInterval != 0 {
		err = q.checkIntervalLimit(expr, maxQueryInterval)
		if err != nil {
			return nil, err
		}
	}

	expr, err = optimizeSampleExpr(expr)
	if err != nil {
		return nil, err
	}

	stepEvaluator, err := q.evaluator.StepEvaluator(ctx, q.evaluator, expr, q.params)
	if err != nil {
		return nil, err
	}
	defer util.LogErrorWithContext(ctx, "closing SampleExpr", stepEvaluator.Close)

	switch stepEvaluator.Type() {
	case VecType:
		return q.aggregateSampleVectors(ctx, stepEvaluator, tenantIDs)
	case TopKVecType:
		return q.aggregateTopKVectors(ctx, stepEvaluator, tenantIDs)
	default:
		return nil, nil
	}
}

func (q *probabilisticQuery) aggregateSampleVectors(
	ctx context.Context,
	stepEvaluator StepEvaluator,
	tenantIDs []string,
) (promql_parser.Value, error) {

	maxSeriesCapture := func(id string) int { return q.limits.MaxQuerySeries(ctx, id) }
	maxSeries := validation.SmallestPositiveIntPerTenant(tenantIDs, maxSeriesCapture)
	seriesIndex := map[uint64]*promql.Series{}

	next, ts, vec := stepEvaluator.Next()
	if stepEvaluator.Error() != nil {
		return nil, stepEvaluator.Error()
	}

	// fail fast for the first step or instant query
	if len(vec) > maxSeries {
		return nil, logqlmodel.NewSeriesLimitError(maxSeries)
	}

	if GetRangeType(q.params) == InstantType {
		sortByValue, err := Sortable(q.params)
		if err != nil {
			return nil, fmt.Errorf("fail to check Sortable, logql: %s ,err: %s", q.params.Query(), err)
		}
		if !sortByValue {
			sort.Slice(vec, func(i, j int) bool { return labels.Compare(vec[i].Metric, vec[j].Metric) < 0 })
		}
		return vec, nil
	}

	stepCount := int(math.Ceil(float64(q.params.End().Sub(q.params.Start()).Nanoseconds()) / float64(q.params.Step().Nanoseconds())))
	if stepCount <= 0 {
		stepCount = 1
	}

	for next {
		for _, p := range vec {
			var (
				series *promql.Series
				hash   = p.Metric.Hash()
				ok     bool
			)

			series, ok = seriesIndex[hash]
			if !ok {
				series = &promql.Series{
					Metric: p.Metric,
					Floats: make([]promql.FPoint, 0, stepCount),
				}
				seriesIndex[hash] = series
			}
			series.Floats = append(series.Floats, promql.FPoint{
				T: ts,
				F: p.F,
			})
		}
		// as we slowly build the full query for each steps, make sure we don't go over the limit of unique series.
		if len(seriesIndex) > maxSeries {
			return nil, logqlmodel.NewSeriesLimitError(maxSeries)
		}

		next, ts, vec = stepEvaluator.Next()
		if stepEvaluator.Error() != nil {
			return nil, stepEvaluator.Error()
		}
	}

	series := make([]promql.Series, 0, len(seriesIndex))
	for _, s := range seriesIndex {
		series = append(series, *s)
	}
	result := promql.Matrix(series)
	sort.Sort(result)

	return result, stepEvaluator.Error()
}

func (q *probabilisticQuery) aggregateTopKVectors(
	ctx context.Context,
	stepEvaluator StepEvaluator,
	tenantIDs []string,
) (promql_parser.Value, error) {
	return nil, nil
}

func (p *ProbabilisticEvaluator) newProbabilisticVectorAggEvaluator(
	ctx context.Context,
	ev SampleEvaluator,
	expr *syntax.VectorAggregationExpr,
	q Params,
) (ProbabilisticStepEvaluator, error) {

	if expr.Operation != syntax.OpTypeTopK {
		return nil, errors.Errorf("unexpected operation: want 'topk', have '%q'", expr.Operation)
	}

	// TODO(karsten): Below is just copy-pasta from newVectorAggEvaluator.
	// We should find better abstractions.

	if expr.Grouping == nil {
		return nil, errors.Errorf("aggregation operator '%q' without grouping", expr.Operation)
	}
	nextEvaluator, err := ev.StepEvaluator(ctx, ev, expr.Left, q)
	if err != nil {
		return nil, err
	}
	lb := labels.NewBuilder(nil)
	buf := make([]byte, 0, 1024)
	sort.Strings(expr.Grouping.Groups)
	return NewProbabilisticStepEvaluator(func() (bool, int64, StepResult) {
		next, ts, vec := nextEvaluator.Next()

		if !next {
			return false, 0, nil
		}
		result := map[uint64]*sketch.Topk{}
		if expr.Params < 1 {
			return next, ts, nil
		}
		for _, s := range vec {
			metric := s.Metric

			var groupingKey uint64
			if expr.Grouping.Without {
				groupingKey, buf = metric.HashWithoutLabels(buf, expr.Grouping.Groups...)
			} else {
				groupingKey, buf = metric.HashForLabels(buf, expr.Grouping.Groups...)
			}
			group, ok := result[groupingKey]
			// Add a new group if it doesn't exist.
			if !ok {
				var m labels.Labels

				if expr.Grouping.Without {
					lb.Reset(metric)
					lb.Del(expr.Grouping.Groups...)
					lb.Del(labels.MetricName)
					m = lb.Labels()
				} else {
					m = make(labels.Labels, 0, len(expr.Grouping.Groups))
					for _, l := range metric {
						for _, n := range expr.Grouping.Groups {
							if l.Name == n {
								m = append(m, l)
								break
							}
						}
					}
					sort.Sort(m)
				}
				// TODO(karsten): get k and c.
				group, err = sketch.NewCMSTopkForCardinality(p.logger, 10, 100000)
				if err != nil {
					// TODO(karsten): return error
					return next, ts, nil
				}
				result[groupingKey] = group
			}

			// TODO(karsten): add s.F instead
			group.Observe(s.Metric.String())
		}
		r := sketch.TopKVector{
			TS: uint64(ts),
		}
		for _, aggr := range result {
			// TODO(karsten): How are we handling groups?
			r.Topk = aggr
		}

		// TODO(karsten): set topkvector values
		return next, ts, TopKVector([]sketch.Topk{})
	}, nextEvaluator.Close, nextEvaluator.Error)
}