package ustc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test executable doubles as a deterministic, non-VPN iWAN subprocess.
func TestMain(tests *testing.M) {
	if os.Getenv("CPA_USTC_IWAN_HELPER") == "1" {
		address := ""
		directory := ""
		fetch := false
		for index, argument := range os.Args {
			if argument == "--fetch" {
				fetch = true
			}
			if argument == "--http-listen" && index+1 < len(os.Args) {
				address = os.Args[index+1]
			}
			if argument == "--config-dir" && index+1 < len(os.Args) {
				directory = os.Args[index+1]
			}
		}
		if fetch {
			if WritePrivate(filepath.Join(directory, "servers.json"), []byte(`{"test":true}`)) != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
		listener, err := net.Listen("tcp", address)
		if err != nil {
			os.Exit(1)
		}
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				os.Exit(0)
			}
			connection.Close()
		}
	}
	os.Exit(tests.Run())
}

func TestCampusWireValues(t *testing.T) {
	for _, test := range []struct {
		raw     string
		campus  bool
		invalid bool
	}{{"1", true, false}, {"0", false, false}, {"true", true, false}, {"false", false, false}, {`"1"`, false, true}, {"null", false, true}, {"2", false, true}, {"", false, true}} {
		t.Run(test.raw, func(t *testing.T) {
			campus, err := parseCampus(json.RawMessage(test.raw))
			if (err != nil) != test.invalid || campus != test.campus {
				t.Fatalf("got %v, %v", campus, err)
			}
		})
	}
}

func TestNetworkDoesNotTrustAmbientProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client := HTTPClient("")
	defer client.CloseIdleConnections()
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("direct transport inherited proxy")
	}
}

func TestAutomaticNetworkFailClosed(t *testing.T) {
	config := DefaultConfig()
	config.StateDir = t.TempDir()
	network := NewNetwork(config)
	defer network.Close()
	network.probe = func(context.Context) (bool, error) { return false, nil }
	if _, err := network.Proxy(context.Background()); err == nil {
		t.Fatal("off-campus requests must not silently go direct")
	}
	network.probe = func(context.Context) (bool, error) { return true, nil }
	network.checked = time.Time{}
	if proxy, err := network.Proxy(context.Background()); err != nil || proxy != "" {
		t.Fatalf("campus path: %q, %v", proxy, err)
	}
}

func TestExplicitProxySkipsCampusProbe(t *testing.T) {
	config := DefaultConfig()
	config.ProxyURL = "socks5h://127.0.0.1:1080"
	network := NewNetwork(config)
	defer network.Close()
	network.probe = func(context.Context) (bool, error) { t.Fatal("unexpected campus probe"); return false, nil }
	if proxy, err := network.Proxy(context.Background()); err != nil || proxy != config.ProxyURL {
		t.Fatalf("got %q, %v", proxy, err)
	}
}

func TestPrivateWriteAndInvalidSettings(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "credentials.json")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("unsafe permissions: %v", stat.Mode())
	}
	for _, raw := range []string{"http://api.llm.ustc.edu.cn/v1", "https://user:secret@api.llm.ustc.edu.cn/v1", "https://api.llm.ustc.edu.cn/v1?token=secret"} {
		config := DefaultConfig()
		config.APIBase = raw
		if config.Validate() == nil {
			t.Fatalf("accepted unsafe URL %s", raw)
		}
	}
}

func TestManagedIwanLoginRestartAndCleanup(t *testing.T) {
	t.Setenv("CPA_USTC_IWAN_HELPER", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultConfig()
	config.Network = "iwan"
	config.StateDir = t.TempDir()
	config.IwanBinary = binary
	network := NewNetwork(config)
	t.Cleanup(network.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = network.Login(ctx, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(filepath.Join(config.StateDir, "iwan", "servers.json"))
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("line credentials: %v %v", stat, err)
	}
	proxy, err := network.Proxy(ctx)
	if err != nil || !strings.HasPrefix(proxy, "http://127.0.0.1:") {
		t.Fatalf("managed proxy: %s %v", proxy, err)
	}
	first := network.process.Process.Pid
	if err = network.process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-network.exited
	if _, err = network.Proxy(ctx); err != nil {
		t.Fatal(err)
	}
	if network.process.Process.Pid == first {
		t.Fatal("crashed child was not restarted")
	}
	finished := network.exited
	network.Close()
	select {
	case <-finished:
	default:
		t.Fatal("child was not reaped")
	}
}
