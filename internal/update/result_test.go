package update

import "testing"

func TestResultIsReportedOnce(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadResult(dir); ok {
		t.Fatal("a result in an empty directory")
	}
	if err := WriteResult(dir, Result{Version: "v1.2.0", Was: "v1.1.0", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	if r, ok := ReadResult(dir); !ok || r.Version != "v1.2.0" || r.Error != "boom" || r.Time.IsZero() {
		t.Fatalf("read: %+v %v", r, ok)
	}
	if r, ok := TakeResult(dir); !ok || r.Was != "v1.1.0" {
		t.Fatalf("take: %+v %v", r, ok)
	}
	if _, ok := TakeResult(dir); ok {
		t.Fatal("taken twice")
	}
}
