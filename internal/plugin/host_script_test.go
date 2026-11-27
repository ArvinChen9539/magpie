package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func TestHostScriptPublishesWithOccupiedOldTemp(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	script := []byte("console.log('host script');\n")
	sum := sha256.Sum256(script)
	dir := filepath.Join(filepath.Dir(catalog.CachePath()), "plugin-host")
	want := filepath.Join(dir, "host-"+hex.EncodeToString(sum[:6])+".js")
	if err := os.MkdirAll(want+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		writers.Go(func() {
			<-start
			path, err := hostScript("host", script)
			if err != nil || path != want {
				t.Errorf("hostScript = %q, %v, want %q", path, err, want)
				return
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(script) {
				t.Errorf("host script = %q, %v, want %q", got, err, script)
			}
		})
	}
	close(start)
	writers.Wait()
}
