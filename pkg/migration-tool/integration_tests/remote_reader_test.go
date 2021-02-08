// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package integration_tests

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

type remoteReadServer struct {
	server *httptest.Server
	series []prompb.TimeSeries
}

// createRemoteReadServer creates a remote read server. It exposes a single /read endpoint and responds with the
// passed series based on the request to the read endpoint. It returns a server which should be closed after
// being used.
func createRemoteReadServer(t *testing.T, seriesToBeSent []prompb.TimeSeries) (*remoteReadServer, string) {
	s := httptest.NewServer(getReadHandler(t, seriesToBeSent))
	return &remoteReadServer{
		server: s,
		series: seriesToBeSent,
	}, s.URL
}

// Series returns the numbr of series in the remoteReadServer.
func (rrs *remoteReadServer) Series() int {
	// Read storage series are immutable. So, no need for read-locks.
	return len(rrs.series)
}

// Samples returns the total number of samples that the remoteReadServer contains.
func (rrs *remoteReadServer) Samples() int {
	// Read storage series are immutable. So, no need for read-locks.
	numSamples := 0
	for _, s := range rrs.series {
		numSamples += len(s.Samples)
	}
	return numSamples
}

// Close closes the server.
func (rrs *remoteReadServer) Close() {
	rrs.server.Close()
}

func getReadHandler(t *testing.T, series []prompb.TimeSeries) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validateReadHeaders(t, w, r) {
			t.Fatal("invalid read headers")
		}

		compressed, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatal("msg", "read header validation error", "err", err.Error())
		}

		reqBuf, err := snappy.Decode(nil, compressed)
		if err != nil {
			t.Fatal("msg", "snappy decode error", "err", err.Error())
		}

		var req prompb.ReadRequest
		if err := proto.Unmarshal(reqBuf, &req); err != nil {
			t.Fatal("msg", "proto unmarshal error", "err", err.Error())
		}
		resp := &prompb.ReadResponse{
			Results: make([]*prompb.QueryResult, len(req.Queries)),
		}

		matchers := req.Queries[0].Matchers
		matchLabels := func(s prompb.TimeSeries) bool {
			for _, l := range s.Labels {
				for _, m := range matchers {
					if m.Type != prompb.LabelMatcher_RE {
						t.Fatalf("unsupported label matcher")
					}

					if m.Name == l.Name {
						re := regexp.MustCompile(m.Value)
						if !re.MatchString(l.Value) {
							return false
						}
					}
				}
			}
			
			return true
		}

		startTs := req.Queries[0].StartTimestampMs
		endTs := req.Queries[0].EndTimestampMs
		ts := make([]*prompb.TimeSeries, len(series)) // Since the response is going to be the number of time-series.
		for i, s := range series {
			ts[i] = &prompb.TimeSeries{}

			if !matchLabels(s) {
				continue
			}

			var samples []prompb.Sample
			for _, sample := range s.Samples {
				if sample.Timestamp >= startTs && sample.Timestamp < endTs {
					// Considering including of time boundaries. Prometheus excludes the end boundary.
					// TODO: check this with the Brian's comments in design doc.
					samples = append(samples, sample)
				}
			}
			if len(samples) > 0 {
				ts[i].Labels = s.Labels
				ts[i].Samples = samples
			}
		}
		if len(resp.Results) == 0 {
			t.Fatal("queries num is 0")
		}
		resp.Results[0] = &prompb.QueryResult{Timeseries: ts}
		data, err := proto.Marshal(resp)
		if err != nil {
			t.Fatal("msg", "internal server error", "err", err.Error())
		}

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set("Content-Encoding", "snappy")

		compressed = snappy.Encode(nil, data)
		if _, err := w.Write(compressed); err != nil {
			t.Fatal("msg", "snappy encode: internal server error", "err", err.Error())
		}
	})
}

func validateReadHeaders(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	// validate headers from https://github.com/prometheus/prometheus/blob/2bd077ed9724548b6a631b6ddba48928704b5c34/storage/remote/client.go
	if r.Method != "POST" {
		t.Fatalf("HTTP Method %s instead of POST", r.Method)
	}

	if !strings.Contains(r.Header.Get("Content-Encoding"), "snappy") {
		t.Fatalf("non-snappy compressed data got: %s", r.Header.Get("Content-Encoding"))
	}

	if r.Header.Get("Content-Type") != "application/x-protobuf" {
		t.Fatal("non-protobuf data")
	}

	remoteReadVersion := r.Header.Get("X-Prometheus-Remote-Read-Version")
	if remoteReadVersion == "" {
		err := "missing X-Prometheus-Remote-Read-Version"
		t.Fatal("msg", "Read header validation error", "err", err)
	} else if !strings.HasPrefix(remoteReadVersion, "0.1.") {
		t.Fatalf("unexpected Remote-Read-Version %s, expected 0.1.X", remoteReadVersion)
	}

	return true
}
