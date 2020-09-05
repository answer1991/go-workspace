package main

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestReadTrackingBodyDataRace(t *testing.T) {
	const (
		urlPostWithGoaway = "/post-with-goaway"
	)

	var (
		requestPostBody = []byte("hello")

		responseBody = requestPostBody
	)

	postHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !reflect.DeepEqual(requestPostBody, reqBody) {
			http.Error(w, fmt.Sprintf("expect request body: %s, got: %s", requestPostBody, reqBody), http.StatusBadRequest)
			return
		}

		// Send a GOAWAY and tear down the TCP connection when idle.
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		w.Write(responseBody)
		return
	})

	mux := http.NewServeMux()
	mux.Handle(urlPostWithGoaway, postHandler)

	s := httptest.NewUnstartedServer(mux)

	http2Options := &http2.Server{}

	if err := http2.ConfigureServer(s.Config, http2Options); err != nil {
		t.Fatalf("failed to configure test server to be HTTP2 server, err: %v", err)
	}

	s.TLS = s.Config.TLSConfig

	s.StartTLS()
	defer s.Close()

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{http2.NextProtoTLS},
	}

	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     tlsConfig,
		MaxIdleConnsPerHost: 25,
	}
	if err := http2.ConfigureTransport(tr); err != nil {
		t.Fatalf("failed to configure http client, err: %v", err)
	}

	client := &http.Client{
		Transport: tr,
	}

	const (
		requestCount = 300
		workers      = 10
	)

	urls := make(chan string, requestCount)
	for i := 0; i < requestCount; i++ {
		urls <- urlPostWithGoaway
	}
	close(urls)

	wg := &sync.WaitGroup{}
	wg.Add(workers)

	reqFn := func(client *http.Client, url string) error {
		req, err := http.NewRequest(http.MethodPost, s.URL+url, bytes.NewReader(requestPostBody))
		if err != nil {
			return fmt.Errorf("unexpect new request error: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("failed request test server, err: %v", err)
		}

		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("failed to read response body and status code is %d, error: %v", resp.StatusCode, err)
			}

			return fmt.Errorf("expect response status code: %d, but got: %d. response body: %s", http.StatusOK, resp.StatusCode, body)
		}

		return nil
	}

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()

			for {
				url, ok := <-urls
				if !ok {
					return
				}

				err := reqFn(client, url)
				if err != nil {
					t.Errorf("failed to request server: %v", err)
				}
			}
		}()
	}

	wg.Wait()
}
