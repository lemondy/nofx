package bnweight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var review20261028Buckets = []struct {
	bucket Bucket
	limit  int
	paths  [2]string
}{
	{BucketData, 1000, [2]string{"/futures/data/openInterestHist", "/futures/data/globalLongShortAccountRatio"}},
	{BucketFundingRate, 500, [2]string{"/fapi/v1/fundingRate", "/fapi/v1/fundingInfo"}},
}

func review20261028Request(ctx context.Context, rt http.RoundTripper, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fapi.binance.com"+path, nil)
	if err != nil {
		return err
	}
	r, err := rt.RoundTrip(req)
	if r != nil && r.Body != nil {
		r.Body.Close()
	}
	return err
}

func review20261028Fill(t *testing.T, rt http.RoundTripper, paths [2]string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if err := review20261028Request(context.Background(), rt, paths[i%len(paths)]); err != nil {
			t.Fatalf("request %d of %d: %v", i+1, count, err)
		}
	}
}

func review20261028Usage(s SnapshotData, bucket Bucket) int {
	if bucket == BucketData {
		return s.Data5m
	}
	return s.FundingRate5m
}

func TestReview20261028BucketBurstsAreBounded(t *testing.T) {
	for _, tc := range review20261028Buckets {
		for _, prio := range []Priority{Low, Normal, Critical} {
			t.Run(tc.bucket.String()+"/"+prio.String(), func(t *testing.T) {
				start := time.Date(2026, 10, 28, 12, 0, 10, 0, time.UTC)
				m, clk, waits := newTestMeter(start)
				sent := 0
				base := rtFunc(func(*http.Request) (*http.Response, error) {
					sent++
					return resp(http.StatusOK, nil), nil
				})
				rt := m.Wrap(base, prio)
				review20261028Fill(t, rt, tc.paths, tc.limit)
				if len(*waits) != 0 || !clk.Now().Equal(start) {
					t.Fatalf("requests within budget must pass immediately: waits=%v", *waits)
				}

				// The next slot is five minutes away, beyond the existing 60s
				// guard bound. Refuse without sending or recording the request.
				err := review20261028Request(context.Background(), rt, tc.paths[0])
				var ce *CooldownError
				if !errors.Is(err, ErrCooldown) || !errors.As(err, &ce) {
					t.Fatalf("over-budget request must return CooldownError/ErrCooldown: %v", err)
				}
				if ce.Status != 0 || ce.Priority != prio || !ce.Until.Equal(start.Add(eventRetention+time.Nanosecond)) {
					t.Fatalf("wrong local limit error: %+v", ce)
				}
				if clk.Now().Sub(start) > maxGuardWait || sent != tc.limit {
					t.Fatalf("guard exceeded its bound or sent over budget: elapsed=%v sent=%d", clk.Now().Sub(start), sent)
				}
				s := m.Snapshot()
				if review20261028Usage(s, tc.bucket) != tc.limit || s.Totals[prio].Requests != tc.limit || s.Totals[prio].Weight != 0 || s.EffectiveUsed != 0 {
					t.Fatalf("rejection changed accounting: %+v", s)
				}
				if _, active := m.InCooldown(); active {
					t.Fatal("one exhausted bucket must not trigger a global cooldown")
				}
				client := &http.Client{Transport: rt}
				if _, err := client.Get("https://fapi.binance.com" + tc.paths[1]); !errors.Is(err, ErrCooldown) {
					t.Fatalf("error must still match through http.Client: %v", err)
				}

				// Exhaustion is independent of the other request pool and IP pool.
				other := "/fapi/v1/fundingRate"
				if tc.bucket == BucketFundingRate {
					other = "/futures/data/openInterestHist"
				}
				for _, path := range []string{other, "/fapi/v1/time"} {
					if err := review20261028Request(context.Background(), rt, path); err != nil {
						t.Fatalf("exhausted %s blocked %s: %v", tc.bucket, path, err)
					}
				}
			})
		}
	}
}

func TestReview20261028BucketCapacitySlides(t *testing.T) {
	for _, tc := range review20261028Buckets {
		t.Run(tc.bucket.String(), func(t *testing.T) {
			start := time.Date(2026, 10, 28, 12, 0, 10, 0, time.UTC)
			m, clk, waits := newTestMeter(start)
			rt := m.Wrap(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(http.StatusOK, nil), nil
			}), Normal)
			review20261028Fill(t, rt, tc.paths, 1)
			clk.Advance(time.Minute)
			review20261028Fill(t, rt, tc.paths, tc.limit-1)
			clk.Advance(eventRetention - time.Minute - 10*time.Second)

			// Only the earliest request expires after this wait. The rest
			// must stay charged, rather than resetting the entire pool.
			review20261028Fill(t, rt, tc.paths, 1)
			wantWait := 10*time.Second + time.Nanosecond + 500*time.Millisecond
			if len(*waits) != 1 || (*waits)[0] != wantWait {
				t.Fatalf("waits=%v, want [%v]", *waits, wantWait)
			}
			s := m.Snapshot()
			if review20261028Usage(s, tc.bucket) != tc.limit || s.Totals[Normal].Requests != tc.limit+1 {
				t.Fatalf("sliding window lost live requests: %+v", s)
			}

			// The next request must wait for the later group to expire.
			review20261028Fill(t, rt, tc.paths, 1)
			if len(*waits) != 2 || (*waits)[1] != maxGuardWait {
				t.Fatalf("later events did not retain their own expiry: %v", *waits)
			}
			if s = m.Snapshot(); review20261028Usage(s, tc.bucket) != 2 || s.Totals[Normal].Requests != tc.limit+2 {
				t.Fatalf("expired requests were not released: %+v", s)
			}
		})
	}
}

func TestReview20261028BucketWindowBoundaryMatchesStats(t *testing.T) {
	for _, tc := range review20261028Buckets {
		t.Run(tc.bucket.String(), func(t *testing.T) {
			m, clk, waits := newTestMeter(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
			m.jitter = func() time.Duration { return 0 }
			rt := m.Wrap(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(http.StatusOK, nil), nil
			}), Low)
			review20261028Fill(t, rt, tc.paths, tc.limit)
			clk.Advance(eventRetention)
			if got := review20261028Usage(m.Snapshot(), tc.bucket); got != tc.limit {
				t.Fatalf("events on the existing inclusive boundary must count: %d", got)
			}
			review20261028Fill(t, rt, tc.paths, 1)
			if len(*waits) != 1 || (*waits)[0] != time.Nanosecond || review20261028Usage(m.Snapshot(), tc.bucket) != 1 {
				t.Fatalf("admission must use the same window as stats: waits=%v snapshot=%+v", *waits, m.Snapshot())
			}
		})
	}
}

func TestReview20261028BucketWaitHonoursContext(t *testing.T) {
	for _, tc := range review20261028Buckets {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%v", tc.bucket, deadline), func(t *testing.T) {
				m, clk, _ := newTestMeter(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
				sent := 0
				rt := m.Wrap(rtFunc(func(*http.Request) (*http.Response, error) {
					sent++
					return resp(http.StatusOK, nil), nil
				}), Normal)
				review20261028Fill(t, rt, tc.paths, tc.limit)
				clk.Advance(eventRetention - 10*time.Second)
				ctx, cancel := context.WithCancel(context.Background())
				want := context.Canceled
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
					want = context.DeadlineExceeded
				}
				defer cancel()
				m.sleep = func(ctx context.Context, d time.Duration) error {
					if d > maxGuardWait {
						t.Fatalf("wait %s exceeded guard bound", d)
					}
					if !deadline {
						cancel()
					}
					return sleepCtx(ctx, d)
				}
				err := review20261028Request(ctx, rt, tc.paths[0])
				if !errors.Is(err, want) || sent != tc.limit || m.Snapshot().Totals[Normal].Requests != tc.limit {
					t.Fatalf("context must stop wait without sending/accounting: err=%v sent=%d", err, sent)
				}
			})
		}
	}
}

func TestReview20261028BucketWaitCannotPassAtGuardCap(t *testing.T) {
	for _, tc := range review20261028Buckets {
		t.Run(tc.bucket.String(), func(t *testing.T) {
			m, clk, waits := newTestMeter(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
			sent := 0
			rt := m.Wrap(rtFunc(func(*http.Request) (*http.Response, error) {
				sent++
				return resp(http.StatusOK, nil), nil
			}), Low)
			review20261028Fill(t, rt, tc.paths, tc.limit)
			clk.Advance(eventRetention - maxGuardWait + time.Second)
			// A clock that stays in the full window must not cause either an
			// unbounded wait or the IP guard's eventual pass-through.
			m.sleep = func(_ context.Context, d time.Duration) error {
				*waits = append(*waits, d)
				return nil
			}
			err := review20261028Request(context.Background(), rt, tc.paths[0])
			var total time.Duration
			for _, d := range *waits {
				total += d
			}
			if !errors.Is(err, ErrCooldown) || total <= 0 || total > maxGuardWait || sent != tc.limit {
				t.Fatalf("full bucket must fail closed within guard bound: err=%v wait=%v sent=%d", err, total, sent)
			}
		})
	}
}

func TestReview20261028BucketAdmissionAlsoChecksIP(t *testing.T) {
	for _, tc := range review20261028Buckets {
		t.Run(tc.bucket.String(), func(t *testing.T) {
			m, _, waits := newTestMeter(time.Date(2026, 10, 28, 12, 0, 20, 0, time.UTC))
			seed := m.Wrap(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(http.StatusOK, map[string]string{HeaderUsedWeight1M: "1600"}), nil
			}), Critical)
			if err := review20261028Request(context.Background(), seed, "/fapi/v1/time"); err != nil {
				t.Fatal(err)
			}
			rt := m.Wrap(rtFunc(func(*http.Request) (*http.Response, error) {
				return resp(http.StatusOK, nil), nil
			}), Low)
			review20261028Fill(t, rt, tc.paths, 1)
			if len(*waits) != 1 || (*waits)[0] != 40*time.Second+500*time.Millisecond || review20261028Usage(m.Snapshot(), tc.bucket) != 1 {
				t.Fatalf("request must also respect IP guard: waits=%v snapshot=%+v", *waits, m.Snapshot())
			}
		})
	}
}

func TestReview20261028ConcurrentBucketReservations(t *testing.T) {
	m, _, _ := newTestMeter(time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC))
	// Freeze the clock; IP requests that need to wait fail via context instead
	// of advancing into a new minute while the other workers are admitting.
	m.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	var sent [3]atomic.Int64
	rt := m.Wrap(rtFunc(func(r *http.Request) (*http.Response, error) {
		_, _, bucket := Classify(r.Method, r.URL.Path, r.URL.Query())
		sent[bucket].Add(1)
		return resp(http.StatusOK, nil), nil
	}), Low)
	paths := []string{"/fapi/v1/time", "/fapi/v1/fundingRate", "/futures/data/openInterestHist"}
	const workers = 48
	start := make(chan struct{})
	failures := make(chan error, workers)
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := 0; i < 120; i++ {
				err := review20261028Request(context.Background(), rt, paths[(worker+i)%len(paths)])
				if err != nil && !errors.Is(err, ErrCooldown) && !errors.Is(err, context.DeadlineExceeded) {
					failures <- err
					return
				}
				if i%20 == 0 {
					s := m.Snapshot()
					if s.Data5m > 1000 || s.FundingRate5m > 500 || s.EffectiveUsed > DefaultLowThreshold {
						failures <- fmt.Errorf("concurrent reservation exceeded a budget: %+v", s)
						return
					}
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	// IP saturation can also hold the zero-weight pools, so only the IP pool
	// must fill completely. Every pool must stay within its independent limit.
	s := m.Snapshot()
	if s.EffectiveUsed != DefaultLowThreshold || s.Data5m > 1000 || s.FundingRate5m > 500 {
		t.Fatalf("concurrent admission exceeded budget: %+v", s)
	}
	if sent[BucketIP1M].Load() != int64(s.EffectiveUsed) || sent[BucketData].Load() != int64(s.Data5m) || sent[BucketFundingRate].Load() != int64(s.FundingRate5m) {
		t.Fatalf("sent requests and reservations differ: sent=%d/%d/%d snapshot=%+v", sent[0].Load(), sent[1].Load(), sent[2].Load(), s)
	}
	if got, want := s.Totals[Low].Requests, s.EffectiveUsed+s.Data5m+s.FundingRate5m; got != want {
		t.Fatalf("cumulative requests=%d, want %d", got, want)
	}
}
