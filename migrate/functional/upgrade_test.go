package functional

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"testing"
)

var (
	binDir         = ".versions"
	v1BinPath      = path.Join(binDir, "1")
	v2BinPath      = path.Join(binDir, "2")
	etcdctlBinPath string
)

func init() {
	os.RemoveAll(binDir)
	if err := os.Mkdir(binDir, 0700); err != nil {
		fmt.Printf("unexpected Mkdir error: %v\n", err)
		os.Exit(1)
	}
	if err := os.Symlink(absPathFromEnv("ETCD_V1_BIN"), v1BinPath); err != nil {
		fmt.Printf("unexpected Symlink error: %v\n", err)
		os.Exit(1)
	}
	if err := os.Symlink(absPathFromEnv("ETCD_V2_BIN"), v2BinPath); err != nil {
		fmt.Printf("unexpected Symlink error: %v\n", err)
		os.Exit(1)
	}
	etcdctlBinPath = os.Getenv("ETCDCTL_BIN")

	mustExist(v1BinPath)
	mustExist(v2BinPath)
	mustExist(etcdctlBinPath)
}

func TestStartNewMember(t *testing.T) {
	tests := []*Proc{
		NewProcWithDefaultFlags(v2BinPath),
		NewProcWithV1Flags(v2BinPath),
		NewProcWithV2Flags(v2BinPath),
	}
	for i, tt := range tests {
		if err := tt.Start(); err != nil {
			t.Fatalf("#%d: Start error: %v", i, err)
		}
		defer tt.Terminate()

		ver, err := checkInternalVersion(tt.URL)
		if err != nil {
			t.Fatalf("#%d: checkVersion error: %v", i, err)
		}
		if ver != "2" {
			t.Errorf("#%d: internal version = %s, want %s", i, ver, "2")
		}
	}
}

func TestStartV2Member(t *testing.T) {
	tests := []*Proc{
		NewProcWithDefaultFlags(v2BinPath),
		NewProcWithV1Flags(v2BinPath),
		NewProcWithV2Flags(v2BinPath),
	}
	for i, tt := range tests {
		// get v2 data dir
		p := NewProcWithDefaultFlags(v2BinPath)
		if err := p.Start(); err != nil {
			t.Fatalf("#%d: Start error: %v", i, err)
		}
		p.Stop()
		tt.SetDataDir(p.DataDir)
		if err := tt.Start(); err != nil {
			t.Fatalf("#%d: Start error: %v", i, err)
		}
		defer tt.Terminate()

		ver, err := checkInternalVersion(tt.URL)
		if err != nil {
			t.Fatalf("#%d: checkVersion error: %v", i, err)
		}
		if ver != "2" {
			t.Errorf("#%d: internal version = %s, want %s", i, ver, "2")
		}
	}
}

func TestStartV1Member(t *testing.T) {
	tests := []*Proc{
		NewProcWithDefaultFlags(v2BinPath),
		NewProcWithV1Flags(v2BinPath),
		NewProcWithV2Flags(v2BinPath),
	}
	for i, tt := range tests {
		// get v1 data dir
		p := NewProcWithDefaultFlags(v1BinPath)
		if err := p.Start(); err != nil {
			t.Fatalf("#%d: Start error: %v", i, err)
		}
		p.Stop()
		tt.SetDataDir(p.DataDir)
		if err := tt.Start(); err != nil {
			t.Fatalf("#%d: Start error: %v", i, err)
		}
		defer tt.Terminate()

		ver, err := checkInternalVersion(tt.URL)
		if err != nil {
			t.Fatalf("#%d: checkVersion error: %v", i, err)
		}
		if ver != "1" {
			t.Errorf("#%d: internal version = %s, want %s", i, ver, "1")
		}
	}
}

func TestUpgradeV1Cluster(t *testing.T) {
	// get v2-desired v1 data dir
	pg := NewProcGroupWithV1Flags(v1BinPath, 3)
	if err := pg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	cmd := exec.Command(etcdctlBinPath, "upgrade", "--peer-url", pg[1].PeerURL)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	t.Logf("wait until etcd exits...")
	if err := pg.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}

	npg := NewProcGroupWithV1Flags(v2BinPath, 3)
	npg.InheritDataDir(pg)
	npg.CleanUnsuppportedV1Flags()
	if err := npg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer npg.Terminate()

	for _, p := range npg {
		ver, err := checkInternalVersion(p.URL)
		if err != nil {
			t.Fatalf("checkVersion error: %v", err)
		}
		if ver != "2" {
			t.Errorf("internal version = %s, want %s", ver, "2")
		}
	}
}

func TestUpgradeV1SnapshotedCluster(t *testing.T) {
	// get v2-desired v1 data dir
	pg := NewProcGroupWithV1Flags(v1BinPath, 3)
	pg.SetSnapCount(10)
	if err := pg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	cmd := exec.Command(etcdctlBinPath, "upgrade", "--peer-url", pg[1].PeerURL)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	t.Logf("wait until etcd exits...")
	if err := pg.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	for _, p := range pg {
		// check it has taken snapshot
		fis, err := ioutil.ReadDir(path.Join(p.DataDir, "snapshot"))
		if err != nil {
			t.Fatalf("unexpected ReadDir error: %v", err)
		}
		if len(fis) == 0 {
			t.Fatalf("unexpected no-snapshot data dir")
		}
	}

	npg := NewProcGroupWithV1Flags(v2BinPath, 3)
	npg.InheritDataDir(pg)
	npg.CleanUnsuppportedV1Flags()
	if err := npg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer npg.Terminate()

	for _, p := range npg {
		ver, err := checkInternalVersion(p.URL)
		if err != nil {
			t.Fatalf("checkVersion error: %v", err)
		}
		if ver != "2" {
			t.Errorf("internal version = %s, want %s", ver, "2")
		}
	}
}

func TestJoinV1Cluster(t *testing.T) {
	pg := NewProcGroupWithV1Flags(v1BinPath, 1)
	if err := pg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	pg.Stop()
	npg := NewProcGroupWithV1Flags(v2BinPath, 3)
	npg[0].SetDataDir(pg[0].DataDir)
	if err := npg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer npg.Terminate()

	for _, p := range npg {
		ver, err := checkInternalVersion(p.URL)
		if err != nil {
			t.Fatalf("checkVersion error: %v", err)
		}
		if ver != "1" {
			t.Errorf("internal version = %s, want %s", ver, "1")
		}
	}
}

func TestJoinV1ClusterViaDiscovery(t *testing.T) {
	dp := NewProcWithDefaultFlags(v1BinPath)
	dp.SetV1Addr("127.0.0.1:5001")
	dp.SetV1PeerAddr("127.0.0.1:8001")
	if err := dp.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer dp.Terminate()

	durl := "http://127.0.0.1:5001/v2/keys/cluster/"
	pg := NewProcGroupViaDiscoveryWithV1Flags(v1BinPath, 1, durl)
	if err := pg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	pg.Stop()
	npg := NewProcGroupViaDiscoveryWithV1Flags(v2BinPath, 3, durl)
	npg[0].SetDataDir(pg[0].DataDir)
	if err := npg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer npg.Terminate()

	for _, p := range npg {
		ver, err := checkInternalVersion(p.URL)
		if err != nil {
			t.Fatalf("checkVersion error: %v", err)
		}
		if ver != "1" {
			t.Errorf("internal version = %s, want %s", ver, "1")
		}
	}
}

func TestUpgradeV1Standby(t *testing.T) {
	// get v1 standby data dir
	pg := NewProcGroupWithV1Flags(v1BinPath, 3)
	if err := pg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	req, err := http.NewRequest("PUT", pg[0].PeerURL+"/v2/admin/config", bytes.NewBufferString(`{"activeSize":3,"removeDelay":1800,"syncInterval":5}`))
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http Do error: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	p := NewProcInProcGroupWithV1Flags(v2BinPath, 4, 3)
	if err := p.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	fmt.Println("checking new member is in standby mode...")
	mustExist(path.Join(p.DataDir, "standby_info"))
	ver, err := checkInternalVersion(p.URL)
	if err != nil {
		t.Fatalf("checkVersion error: %v", err)
	}
	if ver != "1" {
		t.Errorf("internal version = %s, want %s", ver, "1")
	}

	fmt.Println("upgrading the whole cluster...")
	cmd := exec.Command(etcdctlBinPath, "upgrade", "--peer-url", pg[0].PeerURL)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	fmt.Println("waiting until peer-mode etcd exits...")
	if err := pg.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	fmt.Println("restarting the peer-mode etcd...")
	npg := NewProcGroupWithV1Flags(v2BinPath, 3)
	npg.InheritDataDir(pg)
	npg.CleanUnsuppportedV1Flags()
	if err := npg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer npg.Terminate()
	fmt.Println("waiting until standby-mode etcd exits...")
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	fmt.Println("restarting the standby-mode etcd...")
	np := NewProcInProcGroupWithV1Flags(v2BinPath, 4, 3)
	np.SetDataDir(p.DataDir)
	np.CleanUnsuppportedV1Flags()
	if err := np.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer np.Terminate()

	fmt.Println("checking the new member is in v2 proxy mode...")
	ver, err = checkInternalVersion(np.URL)
	if err != nil {
		t.Fatalf("checkVersion error: %v", err)
	}
	if ver != "2" {
		t.Errorf("internal version = %s, want %s", ver, "1")
	}
	if _, err := os.Stat(path.Join(np.DataDir, "proxy")); err != nil {
		t.Errorf("stat proxy dir error = %v, want nil", err)
	}
}

func TestUpgradeV1TLSCluster(t *testing.T) {
	// get v2-desired v1 data dir
	pg := NewProcGroupWithV1Flags(v1BinPath, 3)
	pg.SetPeerTLS("./fixtures/server.crt", "./fixtures/server.key.insecure", "./fixtures/ca.crt")
	if err := pg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	cmd := exec.Command(etcdctlBinPath,
		"upgrade", "--peer-url", pg[1].PeerURL,
		"--peer-cert-file", "./fixtures/server.crt",
		"--peer-key-file", "./fixtures/server.key.insecure",
		"--peer-ca-file", "./fixtures/ca.crt",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}
	t.Logf("wait until etcd exits...")
	if err := pg.Wait(); err != nil {
		t.Fatalf("Wait error: %v", err)
	}

	npg := NewProcGroupWithV1Flags(v2BinPath, 3)
	npg.SetPeerTLS("./fixtures/server.crt", "./fixtures/server.key.insecure", "./fixtures/ca.crt")
	npg.InheritDataDir(pg)
	npg.CleanUnsuppportedV1Flags()
	if err := npg.Start(); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	defer npg.Terminate()

	for _, p := range npg {
		ver, err := checkInternalVersion(p.URL)
		if err != nil {
			t.Fatalf("checkVersion error: %v", err)
		}
		if ver != "2" {
			t.Errorf("internal version = %s, want %s", ver, "2")
		}
	}
}

func absPathFromEnv(name string) string {
	path, err := filepath.Abs(os.Getenv(name))
	if err != nil {
		fmt.Printf("unexpected Abs error: %v\n", err)
	}
	return path
}

func mustExist(path string) {
	if _, err := os.Stat(path); err != nil {
		fmt.Printf("%v\n", err)
		os.Exit(1)
	}
}

func checkInternalVersion(url string) (string, error) {
	resp, err := http.Get(url + "/version")
	if err != nil {
		return "", err
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var m map[string]string
	err = json.Unmarshal(b, &m)
	return m["internalVersion"], err
}
