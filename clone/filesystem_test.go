package clone

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteFilesystemsFollowsEnvOrderAndIgnoresOwnOverride(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, "config", "packages", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("shopware.yaml", "shopware:\n  filesystem:\n    public:\n      type: local\n    theme:\n      type: amazon-s3\n")
	write("prod/shopware.yaml", "shopware:\n  filesystem:\n    public:\n      type: \"amazon-s3\"\n      url: \"%env(S3_URL)%\"\n")
	write("prod/zz-theme.yml", "shopware:\n  filesystem:\n    theme:\n      type: local\n")
	write("dev/shopware.yaml", "shopware:\n  filesystem:\n    private:\n      type: google-storage\n")

	got := RemoteFilesystems(dir, "prod")
	if len(got) != 1 || got["public"] != "amazon-s3" {
		t.Fatalf("prod: got %v, want only public=amazon-s3", got)
	}
	if err := WriteFilesystemOverride(dir, "prod", []string{"public"}); err != nil {
		t.Fatal(err)
	}
	if got := RemoteFilesystems(dir, "prod"); got["public"] != "amazon-s3" {
		t.Fatalf("a rerun must still see the source config, got %v", got)
	}
	if err := WriteFilesystemOverride(dir, "prod", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "packages", "prod", FilesystemOverrideFile)); !os.IsNotExist(err) {
		t.Fatalf("override not removed: %v", err)
	}
}
