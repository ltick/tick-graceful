package graceful

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"log"
)

const (
	// The tests will run a test server on this port.
	port               = 9654
	concurrentRequestN = 8
	killTime           = 500 * time.Millisecond
	timeoutTime        = 1000 * time.Millisecond
	waitTime           = 100 * time.Millisecond
)

func runQuery(t *testing.T, expected int, shouldErr bool, wg *sync.WaitGroup, once *sync.Once) {
	defer wg.Done()
	client := http.Client{}
	r, err := client.Get(fmt.Sprintf("http://localhost:%d", port))
	if shouldErr && err == nil {
		once.Do(func() {
			t.Error("Expected an error but none was encountered.")
		})
	} else if shouldErr && err != nil {
		if checkErr(t, err, once) {
			return
		}
	}
	if r != nil && r.StatusCode != expected {
		once.Do(func() {
			t.Errorf("Incorrect status code on response. Expected %d. Got %d", expected, r.StatusCode)
		})
	} else if r == nil {
		once.Do(func() {
			t.Error("No response when a response was expected.")
		})
	}
}

func checkErr(t *testing.T, err error, once *sync.Once) bool {
	if err.(*url.Error).Err == io.EOF {
		return true
	}
	var errno syscall.Errno
	switch oe := err.(*url.Error).Err.(type) {
	case *net.OpError:
		switch e := oe.Err.(type) {
		case syscall.Errno:
			errno = e
		case *os.SyscallError:
			errno = e.Err.(syscall.Errno)
		}
		if errno == syscall.ECONNREFUSED {
			return true
		} else if err != nil {
			once.Do(func() {
				t.Error("Error on Get:", err)
			})
		}
	default:
		if strings.Contains(err.Error(), "transport closed before response was received") {
			return true
		}
		if strings.Contains(err.Error(), "server closed connection") {
			return true
		}
		fmt.Printf("unknown err: %s, %#v\n", err, err)
	}
	return false
}

func createListener(sleep time.Duration) (*http.Server, net.Listener, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		time.Sleep(sleep)
		rw.WriteHeader(http.StatusOK)
	})

	server := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	return server, l, err
}

func launchTestQueries(t *testing.T, wg *sync.WaitGroup, c chan os.Signal) {
	defer wg.Done()
	var once sync.Once

	for i := 0; i < concurrentRequestN; i++ {
		wg.Add(1)
		go runQuery(t, http.StatusOK, false, wg, &once)
	}

	time.Sleep(waitTime)
	c <- os.Interrupt
	time.Sleep(waitTime)

	for i := 0; i < concurrentRequestN; i++ {
		wg.Add(1)
		go runQuery(t, 0, true, wg, &once)
	}
}

func TestGracefulRun(t *testing.T) {
	var wg sync.WaitGroup
	defer wg.Wait()

	c := make(chan os.Signal, 1)
	server, l, err := createListener(killTime / 2)
	if err != nil {
		t.Fatal(err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		g := New()
		srv := g.Server(server).Timeout(killTime).LogFunc(log.Printf).Build()
		srv.Serve(l)
	}()

	wg.Add(1)
	go launchTestQueries(t, &wg, c)
}
