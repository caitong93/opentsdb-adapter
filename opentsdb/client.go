// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/kit/log/level"

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
			fmt.Println(string(rawBytes))

			resp, err := ctxhttp.Post(ctx, c.client, c.url+queryEndpoint, contentTypeJSON, bytes.NewBuffer(rawBytes))
			if err != nil {
				level.Warn(c.logger).Log("falied to send request to opentsdb")
				errCh <- err
				return
			}
			defer resp.Body.Close()
			rawBytes, err = ioutil.ReadAll(resp.Body)
			if err != nil {
				errCh <- err
				return
			}
			if resp.StatusCode != 200 {
				level.Warn(c.logger).Log(fmt.Sprintf("query opentsdb error: %s", string(rawBytes)))
				errCh <- fmt.Errorf("got status code %v", resp.StatusCode)
				return
			}
			var res otdbQueryResSet

			if err = json.Unmarshal(rawBytes, &res); err != nil {
				errCh <- err
				return
			}
			l.Lock()
			err = mergeResult(labelsToSeries, &res)
			l.Unlock()
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

func concatLabels(labels map[string]TagValue) string {
	// 0xff cannot cannot occur in valid UTF-8 sequences, so use it
	// as a separator here.
	separator := "\xff"
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+separator+string(v))
	}
	return strings.Join(pairs, separator)
}

func tagsToLabelPairs(name string, tags map[string]TagValue) []*prompb.Label {
	pairs := make([]*prompb.Label, 0, len(tags))
	for k, v := range tags {
		pairs = append(pairs, &prompb.Label{
			Name:  k,
			Value: string(v),
		})
	}
	pairs = append(pairs, &prompb.Label{
		Name:  model.MetricNameLabel,
		Value: name,
	})
	return pairs
}

func mergeResult(labelsToSeries map[string]*prompb.TimeSeries, results *otdbQueryResSet) error {
	for _, r := range *results {
		k := concatLabels(r.Tags)
		ts, ok := labelsToSeries[k]
		if !ok {
			ts = &prompb.TimeSeries{
				Labels: tagsToLabelPairs(string(r.Metric), r.Tags),
			}
			labelsToSeries[k] = ts
		}
		ts.Samples = mergeSamples(ts.Samples, valuesToSamples(r.DPs))
	}
	return nil
}

func valuesToSamples(dps otdbDPs) []*prompb.Sample {
	samples := make([]*prompb.Sample, 0, len(dps))
	for t, v := range dps {
		samples = append(samples, &prompb.Sample{
			Timestamp: t * 1000,
			Value:     v,
		})
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Timestamp != samples[j].Timestamp {
			return samples[i].Timestamp < samples[j].Timestamp
		}
		return samples[i].Value < samples[j].Value
	})
	return samples
}

// mergeSamples merges two lists of sample pairs and removes duplicate
// timestamps. It assumes that both lists are sorted by timestamp.
func mergeSamples(a, b []*prompb.Sample) []*prompb.Sample {
	result := make([]*prompb.Sample, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Timestamp < b[j].Timestamp {
			result = append(result, a[i])
			i++
		} else if a[i].Timestamp > b[j].Timestamp {
			result = append(result, b[j])
			j++
		} else {
			result = append(result, a[i])
			i++
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

func (c *Client) buildQueryReq(q *prompb.Query) (*otdbQueryReq, error) {
	req := otdbQueryReq{
		Start: q.GetStartTimestampMs() / 1000,
		End:   q.GetEndTimestampMs() / 1000,
	}

	qr := otdbQuery{
		Aggregator: "none",
	}
	for _, m := range q.Matchers {
		if m.Name == model.MetricNameLabel {
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				qr.Metric = TagValue(m.Value)
			default:
				// TODO: Figure out how to support these efficiently.
				return nil, fmt.Errorf("regex, non-equal or regex-non-equal matchers are not supported on the metric name yet")
			}
			continue
		}
		ft := otdbFilter{
			GroupBy: true,
			Tagk:    m.Name,
		}
		switch m.Type {
		case prompb.LabelMatcher_EQ:
			ft.Type = otdbFilterTypeLiteralOr
			ft.Filter = toTagValue(m.Value)
		case prompb.LabelMatcher_NEQ:
			ft.Type = otdbFilterTypeNotLiteralOr
			ft.Filter = toTagValue(m.Value)
		case prompb.LabelMatcher_RE:
			return nil, fmt.Errorf("unsupported match type RE")
		case prompb.LabelMatcher_NRE:
			return nil, fmt.Errorf("unsupported match type NRE")
		default:
			return nil, fmt.Errorf("unknown match type %v", m.Type)
		}
		qr.Filters = append(qr.Filters, ft)
	}

	req.Queries = append(req.Queries, qr)
	return &req, nil
}

// Name identifies the client as an OpenTSDB client.
func (c *Client) Name() string {
	return "opentsdb"
}
