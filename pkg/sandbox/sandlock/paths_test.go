package sandlock

import "testing"

func TestCleanSandboxPath(t *testing.T) {
	cases := []struct {
		path    string
		wantErr bool
	}{
		{"/testbed", false},
		{"/", false},
		{"", true},
		{"relative", true},
		{"/a/../b", true},
		{"/a/./b", true},
		{"/a//b", true},
		{"/a\x00b", true},
	}
	for _, c := range cases {
		_, err := cleanSandboxPath(c.path)
		if (err != nil) != c.wantErr {
			t.Errorf("cleanSandboxPath(%q) error = %v, wantErr %v", c.path, err, c.wantErr)
		}
	}
}

func TestRequireNonRoot(t *testing.T) {
	if _, err := requireNonRoot("/"); err == nil {
		t.Error("requireNonRoot(\"/\") should reject the sandbox root")
	}
	if _, err := requireNonRoot("/testbed"); err != nil {
		t.Errorf("requireNonRoot(/testbed) = %v, want nil", err)
	}
}

func TestOutputPath(t *testing.T) {
	root := t.TempDir()
	if _, err := outputPath(root, root+"/../escape.txt"); err == nil {
		t.Error("outputPath should reject a destination outside root")
	}
	resolved, err := outputPath(root, root+"/inside/file.txt")
	if err != nil {
		t.Fatalf("outputPath(inside) = %v, want nil", err)
	}
	if resolved == "" {
		t.Error("outputPath returned an empty path")
	}
}
