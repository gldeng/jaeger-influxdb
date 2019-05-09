package storev2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/influxdata/flux"
	"github.com/influxdata/flux/csv"
	"github.com/influxdata/influxdb"
	"github.com/influxdata/jaeger-store/common"
	"github.com/influxdata/jaeger-store/dbmodel"
	"github.com/influxdata/jaeger-store/influx2http"
	"github.com/jaegertracing/jaeger/model"
	"github.com/jaegertracing/jaeger/storage/spanstore"
	"go.uber.org/zap"
)

var _ spanstore.Reader = (*Reader)(nil)

// Reader can query for and load traces from InfluxDB v2.x.
type Reader struct {
	fluxService     *influx2http.FluxService
	orgID           influxdb.ID
	bucket          string
	spanMeasurement string
	logMeasurement  string
	defaultLookback time.Duration

	resultDecoder *csv.ResultDecoder

	logger *zap.Logger
}

// NewReader returns a new SpanReader for InfluxDB v2.x.
func NewReader(fluxService *influx2http.FluxService, orgID influxdb.ID, bucket, spanMeasurement, logMeasurement string, defaultLookback time.Duration, logger *zap.Logger) *Reader {
	return &Reader{
		resultDecoder:   csv.NewResultDecoder(csv.ResultDecoderConfig{}),
		fluxService:     fluxService,
		orgID:           orgID,
		bucket:          bucket,
		spanMeasurement: spanMeasurement,
		logMeasurement:  logMeasurement,
		defaultLookback: defaultLookback,
		logger:          logger,
	}
}

func (r *Reader) query(ctx context.Context, fluxQuery string) (flux.Result, error) {
	println(fluxQuery)
	queryRequest := influx2http.QueryRequest{
		Query: fluxQuery,
		Org:   &influxdb.Organization{ID: r.orgID},
		Dialect: influx2http.QueryDialect{
			Annotations: []string{"group", "datatype", "default"},
		},
	}.WithDefaults()

	proxyRequest, err := queryRequest.ProxyRequest()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if _, err = r.fluxService.Query(ctx, &buf, proxyRequest); err != nil {
		return nil, err
	}

	return r.resultDecoder.Decode(&buf)
}

const queryGetServicesFlux = `
import "influxdata/influxdb/v1"
v1.measurementTagValues(bucket: "%s", measurement: "%s", tag: "%s")
`

// GetServices returns all services traced by Jaeger
func (r *Reader) GetServices(ctx context.Context) ([]string, error) {
	println("GetServices called")

	result, err := r.query(ctx, fmt.Sprintf(queryGetServicesFlux, r.bucket, r.spanMeasurement, common.ServiceNameKey))
	if err != nil {
		if err == io.EOF {
			err = nil
		}
		return nil, err
	}
	var services []string
	err = result.Tables().Do(func(table flux.Table) error {
		return table.Do(func(reader flux.ColReader) error {
			for rowI := 0; rowI < reader.Len(); rowI++ {
				services = append(services, reader.Strings(0).ValueString(rowI))
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return services, nil
}

const queryGetOperationsFlux = `
import "influxdata/influxdb/v1"
v1.tagValues(bucket:"%s", tag:"%s", predicate: (r) => r._measurement=="%s" and r.%s=="%s")
`

// GetOperations returns all operations for a specific service traced by Jaeger
func (r *Reader) GetOperations(ctx context.Context, service string) ([]string, error) {
	println("GetOperations called")

	q := fmt.Sprintf(queryGetOperationsFlux, r.bucket, common.OperationNameKey, r.spanMeasurement, common.ServiceNameKey, service)
	result, err := r.query(ctx, q)
	if err != nil {
		if err == io.EOF {
			err = nil
		}
		return nil, err
	}

	var operations []string
	err = result.Tables().Do(func(table flux.Table) error {
		return table.Do(func(reader flux.ColReader) error {
			for rowI := 0; rowI < reader.Len(); rowI++ {
				operations = append(operations, reader.Strings(0).ValueString(rowI))
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return operations, nil
}

// GetTrace takes a traceID and returns a Trace associated with that traceID
func (r *Reader) GetTrace(ctx context.Context, traceID model.TraceID) (*model.Trace, error) {
	println("GetTrace called")

	result, err := r.query(ctx,
		dbmodel.NewFluxTraceQuery(r.bucket, r.spanMeasurement, r.logMeasurement, time.Now().Add(r.defaultLookback)).BuildTraceQuery([]model.TraceID{traceID}))
	if err != nil {
		if err == io.EOF {
			err = nil
		}
		return nil, err
	}

	traces, err := dbmodel.TracesFromFluxResult(result, r.spanMeasurement, r.logMeasurement, r.logger)
	if err != nil {
		return nil, err
	}
	if len(traces) == 0 {
		return nil, spanstore.ErrTraceNotFound
	}
	if len(traces) > 1 {
		panic("more than one trace returned, expected exactly one; bug in query?")
	}

	return traces[0], nil
}

// FindTraces retrieve traces that match the traceQuery
func (r *Reader) FindTraces(ctx context.Context, query *spanstore.TraceQueryParameters) ([]*model.Trace, error) {
	println("FindTraces called")

	traceIDs, err := r.FindTraceIDs(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(traceIDs) == 0 {
		return nil, nil
	}

	tq := dbmodel.NewFluxTraceQuery(r.bucket, r.spanMeasurement, r.logMeasurement, query.StartTimeMin)
	if !query.StartTimeMax.IsZero() {
		tq.StartTimeMax(query.StartTimeMax)
	}
	result, err := r.query(ctx, tq.BuildTraceQuery(traceIDs))
	if err != nil {
		if err == io.EOF {
			err = nil
		}
		return nil, err
	}
	traces, err := dbmodel.TracesFromFluxResult(result, r.spanMeasurement, r.logMeasurement, r.logger)
	if err != nil {
		return nil, err
	}

	return traces, nil
}

// FindTraceIDs retrieve traceIDs that match the traceQuery
func (r *Reader) FindTraceIDs(ctx context.Context, query *spanstore.TraceQueryParameters) ([]model.TraceID, error) {
	println("FindTraceIDs called")

	q := dbmodel.FluxTraceQueryFromTQP(r.bucket, r.spanMeasurement, r.logMeasurement, query)
	result, err := r.query(ctx, q.BuildTraceIDQuery())
	if err != nil {
		if err == io.EOF {
			err = nil
		}
		return nil, err
	}

	return dbmodel.TraceIDsFromFluxResult(result)
}

var getDependenciesQueryFlux = fmt.Sprintf(`
from(bucket: "%%s")
 |> range(start: %%s, stop: %%s)
 |> filter(fn: (r) => r._measurement == "%%s" and (r._field == "%s" or r._field == "%s"))
 |> pivot(rowKey:["_time"], columnKey: ["_field"], valueColumn: "_value")
 |> group()
 |> keep(columns: ["%s", "%s", "%s"])
`, "span_id", "references", "span_id", "references", "service_name")

// GetDependencies returns all inter-service dependencies
func (r *Reader) GetDependencies(endTs time.Time, lookback time.Duration) ([]model.DependencyLink, error) {
	println("GetDependencies called")

	result, err := r.query(context.TODO(),
		fmt.Sprintf(getDependenciesQueryFlux,
			r.bucket, endTs.Add(-1 * lookback).UTC().Format(time.RFC3339Nano), endTs.UTC().Format(time.RFC3339Nano), r.spanMeasurement))
	if err != nil {
		if err == io.EOF {
			err = nil
		}
		return nil, err
	}

	return dbmodel.DependencyLinksFromResultV2(result)
}