package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/googlecloudrobotics/core/src/go/cmd/http-relay-client/client"
	"github.com/googlecloudrobotics/core/src/go/cmd/http-relay-server/server"
)

type TestCase struct {
	payloadSize int
	blockSize   int
}

type TestFixture struct {
	backendUrl  string
	relayUrl    string
	stopServers context.CancelFunc
}

// discoverRelayServerPort scans the server logs to find the actual port the server is listening on.
// Used when starting the server with port `0` (let OS choose any free port).
func discoverRelayServerPort() (int, error) {
	var buf bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(originalOutput)

	type portResult struct {
		port int
		err  error
	}
	portChan := make(chan portResult)
	go func() {
		re, err := regexp.Compile(`Relay server listening on: 127.0.0.1:(\d+)\n`)
		if err != nil {
			portChan <- portResult{0, err}
			return
		}
		for {
			l := buf.String()
			match := re.FindStringSubmatch(l)
			if len(match) > 1 {
				port, err := strconv.Atoi(match[1])
				if err != nil {
					portChan <- portResult{0, err}
					return
				}
				portChan <- portResult{port, nil}
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	select {
	case pr := <-portChan:
		if pr.err != nil {
			return 0, pr.err
		}
		return pr.port, nil
	case <-time.After(10 * time.Second):
		return 0, errors.New("timed out waiting for relay server")
	}
}

func newTestFixture(b *testing.B, tc TestCase) TestFixture {
	ctx, cancel := context.WithCancel(context.Background())
	payload := make([]byte, tc.payloadSize)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))

	relayServer := server.NewServer()
	go relayServer.Start(0, tc.blockSize)
	go func() {
		<-ctx.Done()
		relayServer.Stop()
	}()
	relayPort, err := discoverRelayServerPort()
	if err != nil {
		b.Fatal(err)
	}
	relayUrl := fmt.Sprint("http://127.0.0.1:", relayPort, "/client/test-server/")

	// suppress logs from relays
	log.SetOutput(io.Discard)

	clientConfig := client.DefaultClientConfig()
	clientConfig.RelayScheme = "http"
	clientConfig.RelayAddress = fmt.Sprint("127.0.0.1:", relayPort)
	clientConfig.BackendScheme = "http"
	clientConfig.BackendAddress = fmt.Sprint("127.0.0.1:", ts.Listener.Addr().(*net.TCPAddr).Port)
	clientConfig.DisableAuthForRemote = true
	clientConfig.ServerName = "test-server"
	relayClient := client.NewClient(clientConfig)
	go relayClient.Start()
	go func() {
		<-ctx.Done()
		relayClient.Stop()
	}()

	return TestFixture{backendUrl: ts.URL, relayUrl: relayUrl, stopServers: cancel}
}

func benchmarkHttpRelay(b *testing.B, url string) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHttpRelay(b *testing.B) {
	tests := []struct {
		payloadSize int
		blockSize   int
	}{
		{payloadSize: 0, blockSize: 10 * 1024},
		{payloadSize: 10 * 1024, blockSize: 10 * 1024},
		{payloadSize: 100 * 1024, blockSize: 10 * 1024},
	}

	for _, tc := range tests {
		tf := newTestFixture(b, tc)
		b.Run(fmt.Sprintf("BenchmarkHttpRelayDirect_Payload%d", tc.payloadSize), func(b *testing.B) {
			benchmarkHttpRelay(b, tf.backendUrl)
		})
		b.Run(fmt.Sprintf("BenchmarkHttpRelay_Payload%d", tc.payloadSize), func(b *testing.B) {
			benchmarkHttpRelay(b, tf.relayUrl)
		})
		tf.stopServers()
	}
}
