package opentsdb

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"golang.org/x/net/context/ctxhttp"
)

const (
	putEndpoint     = "/api/put"
	queryEndpoint   = "/api/query"
	contentTypeJSON = "application/json"
)

func getDefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
}

// Client allows sending batches of Prometheus samples to OpenTSDB.
type Client struct {
	logger  log.Logger
	client  *http.Client
	url     string
	timeout time.Duration
}

// NewClient creates a new Client.
func NewClient(logger log.Logger, url string, timeout time.Duration) *Client {
	return &Client{
		client: &http.Client{
			Transport: getDefaultTransport(),
		},
		logger:  logger,
		url:     url,
		timeout: timeout,
	}
}

// StoreSamplesRequest is used for building a JSON request for storing samples
// via the OpenTSDB.
type StoreSamplesRequest struct {
	Metric    TagValue            `json:"metric"`
	Timestamp int64               `json:"timestamp"`
	Value     float64             `json:"value"`
	Tags      map[string]TagValue `json:"tags"`
}

// tagsFromMetric translates Prometheus metric into OpenTSDB tags.
func tagsFromMetric(m model.Metric) map[string]TagValue {
	tags := make(map[string]TagValue, len(m)-1)
	for l, v := range m {
		if l == model.MetricNameLabel {
			continue
		}
		if v == "" {
			v = defaultEmptyTagValue
		}
		tags[string(l)] = TagValue(v)
	}
	return tags
}

// Write sends a batch of samples to OpenTSDB via its HTTP API.
func (c *Client) Write(samples model.Samples) error {
	reqs := make([]StoreSamplesRequest, 0, len(samples))
	for _, s := range samples {
		v := float64(s.Value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			// level.Debug(c.logger).Log("msg", "cannot send value to OpenTSDB, skipping sample", "value", v, "sample", s)
			continue
		}
		metric := TagValue(s.Metric[model.MetricNameLabel])
		reqs = append(reqs, StoreSamplesRequest{
			Metric:    metric,
			Timestamp: s.Timestamp.Unix(),
			Value:     v,
			Tags:      tagsFromMetric(s.Metric),
		})
	}

	if len(reqs) == 0 {
		return nil
	}

	u, err := url.Parse(c.url)
	if err != nil {
		return err
	}

	u.Path = putEndpoint
	u.RawQuery = "summary"
	// u.RawQuery = "details"

	buf, err := json.Marshal(reqs)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	resp, err := ctxhttp.Post(ctx, c.client, u.String(), contentTypeJSON, bytes.NewBuffer(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// API returns status code 204 for successful writes.
	// http://opentsdb.net/docs/build/html/api_http/put.html
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// API returns status code 400 on error, encoding error details in the
	// response content in JSON.
	buf, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	fmt.Printf("failed to write samples to OpenTSDB: %s\n", string(buf))
	return fmt.Errorf("failed to write samples to OpenTSDB, get code %v", resp.StatusCode)
}

func (c *Client) Read(req *prompb.ReadRequest) (*prompb.ReadResponse, error) {
	queryReqs := make([]*otdbQueryReq, 0, len(req.Queries))
	for _, q := range req.Queries {
		res, err := c.buildQueryReq(q)
		if err != nil {
			return nil, err
		}
		queryReqs = append(queryReqs, res)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	errCh := make(chan error, 1)
	defer close(errCh)
	var l sync.Mutex
	labelsToSeries := map[string]*prompb.TimeSeries{}
	for i := range queryReqs {
		go func(queryReq *otdbQueryReq) {
			select {
			case <-ctx.Done():
				return
			default:
			}

			rawBytes, err := json.Marshal(queryReq)
			if err != nil {
				errCh <- err
				return
			}

			resp, err := ctxhttp.Post(ctx, c.client, c.url+queryEndpoint, contentTypeJSON, bytes.NewBuffer(rawBytes))
			if err != nil {
				errCh <- err
				return
			}
			if resp.StatusCode != 200 {
				errCh <- fmt.Errorf("got status code %v", resp.StatusCode)
				return
			}
			var res otdbQueryRes
			rawBytes, err = ioutil.ReadAll(resp.Body)
			if err != nil {
				errCh <- err
				return
			}
			if err = json.Unmarshal(rawBytes, &res); err != nil {
				errCh <- err
				return
			}
			l.Lock()
			defer l.Unlock()
			if err = mergeResult(labelsToSeries, &res); err != nil {
				errCh <- err
			}
			errCh <- nil
		}(queryReqs[i])
	}

loop:
	for {
		count := 0
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-errCh:
			if err != nil {
				return nil, err
			}
			count++
			if count == len(queryReqs) {
				break loop
			}
		default:
		}
	}

	resp := prompb.ReadResponse{
		Results: []*prompb.QueryResult{
			{Timeseries: make([]*prompb.TimeSeries, 0, len(labelsToSeries))},
		},
	}
	for _, ts := range labelsToSeries {
		resp.Results[0].Timeseries = append(resp.Results[0].Timeseries, ts)
	}
	return &resp, nil
}

func mergeResult(labelsToSeries map[string]*prompb.TimeSeries, results *otdbQueryRes) error {
	return nil
}

func (c *Client) buildQueryReq(q *prompb.Query) (*otdbQueryReq, error) {
	req := otdbQueryReq{
		Start: q.GetStartTimestampMs() / 1000
		End: q.GetEndTimestampMs() / 1000,
	}

	qr := &otdbQuery{
		Aggregator: "none",
	}
	for _, m := range q.Matchers {
		if m.Name == model.MetricNameLabel {
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				qr.Metric = m.Value
			default:
				// TODO: Figure out how to support these efficiently.
				return nil, fmt.Errorf("regex, non-equal or regex-non-equal matchers are not supported on the metric name yet")
			}
			continue
		}
		ft := &otdbFilter{
			GroupBy: true,
			Tagk: m.Name,
		}
		switch m.Type {
		case prompb.LabelMatcher_EQ:
			matchers = append(matchers, fmt.Sprintf("%q = '%s'", m.Name, escapeSingleQuotes(m.Value)))
		case prompb.LabelMatcher_NEQ:
			matchers = append(matchers, fmt.Sprintf("%q != '%s'", m.Name, escapeSingleQuotes(m.Value)))
		case prompb.LabelMatcher_RE:
			matchers = append(matchers, fmt.Sprintf("%q =~ /^%s$/", m.Name, escapeSlashes(m.Value)))
		case prompb.LabelMatcher_NRE:
			matchers = append(matchers, fmt.Sprintf("%q !~ /^%s$/", m.Name, escapeSlashes(m.Value)))
		default:
			return "", fmt.Errorf("unknown match type %v", m.Type)
		}
	}

	return nil, nil
}

func escapeSlashes(str string) string {
	return strings.Replace(str, `/`, `\/`, -1)
}

// Name identifies the client as an OpenTSDB client.
func (c *Client) Name() string {
	return "opentsdb"
}

type otdbQueryResSet []otdbQueryRes

type otdbQueryRes struct {
	Metric TagValue `json:"metric"`
	// A list of tags only returned when the results are for a single time series.
	// If results are aggregated, this value may be null or an empty map
	Tags map[string]TagValue `json:"tags"`
	// If more than one timeseries were included in the result set, i.e. they were
	// aggregated, this will display a list of tag names that were found in common across all time series.
	AggregatedTags map[string]TagValue `json:"aggregatedTags"`
	DPs            otdbDPs           `json:"dps"`
}

type otdbDPs map[int64]float64

type otdbQueryReq struct {
	Start   int64       `json:"start"`
	End     int64       `json:"end"`
	Queries []otdbQuery `json:"queries"`
}

type otdbQuery struct {
	Metric     string       `json:"metric"`
	Filters    []otdbFilter `json:"filters"`
	Aggregator string       `json:"aggregator"`
}

type otdbFilterType string

const (
	otdbFilterTypeLiteralOr    = "literal_or"
	otdbFilterTypeNotLiteralOr = "not_literal_or"
)

type otdbFilter struct {
	Type    otdbFilterType `json:"type"`
	Tagk    string         `json:"tagk"`
	Filter  string         `json:"filter"`
	GroupBy bool         `json:"groupBy"`
}
