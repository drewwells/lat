package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

var ep string
var timeout time.Duration

// maximum allowed parallel HTTP threads
var max int = 10

func init() {
	flag.StringVar(&ep, "ep", "", "endpoint for testing ie. localhost")
	flag.DurationVar(&timeout, "timeout", 3*time.Second, "Time to run ie. 3s")
}

func main() {
	flag.Parse()

	process(ep, timeout, max)
}

type work struct {
	req *http.Request
	cli *http.Client
}

func (w *work) Do() Result {
	now := time.Now()
	resp, err := w.cli.Do(w.req)
	if err != nil {
		return Result{err: err}
	}
	// bs, _ := ioutil.ReadAll(resp.Body)
	// fmt.Println("found:", string(bs))
	defer resp.Body.Close()

	return Result{
		Code: uint16(resp.StatusCode),
		clen: resp.ContentLength,
		dur:  time.Since(now),
	}
}

type Result struct {
	err  error
	clen int64
	dur  time.Duration

	Code        uint16
	FanDuration []int64
	ReadTime    time.Duration
	Server      string
	ReadTimes   string
}

type manager struct {
	todo chan work
	// Responses with 200
	fin chan Result
	// Responses that errored
	err     chan Result
	closing chan *struct{}
	// Notify when last worker completed
	last  chan *struct{}
	done  chan *struct{}
	limit int

	res    []Result
	errres []Result
}

func process(ep string, dur time.Duration, limit int) {
	m := &manager{
		todo: make(chan work),
		fin:  make(chan Result),
		err:  make(chan Result),

		last:    make(chan *struct{}),
		done:    make(chan *struct{}),
		closing: make(chan *struct{}),
	}

	go m.collect()
	go m.load(ep)
	go m.workers(limit)

	<-time.After(dur)
	close(m.closing)

	<-m.done
	report(m.res, "final", false)
	if len(m.errres) > 0 {
		sortErrors(m.errres)
	}
	return

}

func sortErrors(rs []Result) {
	m := make(map[string]int)
	for _, r := range rs {
		e := r.err.Error()
		if _, ok := m[e]; !ok {
			m[e] = 0
		}
		m[e] = m[e] + 1
	}
	fmt.Println("Errors reported:")
	for k, i := range m {
		fmt.Printf("(%d) %s\n", i, k)
	}
}

func (m *manager) collect() {
	results := make([]Result, 0, 10000)
	errs := make([]Result, 0, 1000)
	for {
		select {
		case r := <-m.fin:
			if r.err == nil && r.clen == 0 {
				r.err = errors.New("empty response")
			}

			if r.err != nil {
				errs = append(errs, r)
				if len(errs) > 100 && len(errs) > len(results) {
					sortErrors(errs)
					log.Fatal("Exited early: more errors than successes from API")
				}
			} else {
				results = append(results, r)
			}
		case <-m.last:
			m.res = results
			m.errres = errs
			close(m.done)
			return
		}
	}
}

func (m *manager) load(ep string) error {
	_, err := http.NewRequest("GET", ep, nil)
	if err != nil {
		// Practicie request, if it fails do nothing
		return err
	}
	cli := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	for {
		// Requests can't be reused, add one to each worker
		req, _ := http.NewRequest("GET", ep, nil)
		w := work{
			req: req,
			cli: cli,
		}
		select {
		case <-m.closing:
			return nil
		case m.todo <- w:
		}
	}
}

func (m *manager) workers(lim int) {
	mu := sync.Mutex{}
	c := sync.NewCond(&mu)

	wg := sync.WaitGroup{}

	var cur int
	defer func() {
		// Before closing, wait for all workers to finish
		wg.Done()
		fmt.Println("DONE!")
		close(m.last)
	}()

	go func() {
		for {
			select {
			case w := <-m.todo:
				mu.Lock()
				for cur == lim {
					c.Wait()
				}
				cur = cur + 1
				mu.Unlock()
				wg.Add(1)
				go func() {
					res := w.Do()

					mu.Lock()
					cur = cur - 1
					mu.Unlock()
					m.fin <- res
					wg.Done()
					c.Signal()
				}()

			}
		}
	}()

	select {
	case <-m.closing:
		fmt.Println("returned")
		return
	}

}
