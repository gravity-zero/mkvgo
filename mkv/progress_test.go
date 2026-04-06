package mkv

import "testing"

func TestProgressFrom_Nil(t *testing.T) {
	got := ProgressFrom(nil)
	if got != nil {
		t.Error("expected nil from nil opts")
	}
}

func TestProgressFrom_Empty(t *testing.T) {
	got := ProgressFrom([]Options{})
	if got != nil {
		t.Error("expected nil from empty opts")
	}
}

func TestProgressFrom_NilProgress(t *testing.T) {
	got := ProgressFrom([]Options{{}})
	if got != nil {
		t.Error("expected nil when Progress field is nil")
	}
}

func TestProgressFrom_Set(t *testing.T) {
	called := false
	fn := func(_, _ int64) { called = true }
	got := ProgressFrom([]Options{{Progress: fn}})
	if got == nil {
		t.Fatal("expected non-nil func")
	}
	got(0, 0)
	if !called {
		t.Error("expected func to be called")
	}
}

func TestFSFrom_Nil(t *testing.T) {
	got := FSFrom(nil)
	if got != nil {
		t.Error("expected nil from nil opts")
	}
}

func TestFSFrom_Empty(t *testing.T) {
	got := FSFrom([]Options{})
	if got != nil {
		t.Error("expected nil from empty opts")
	}
}

func TestFSFrom_NilFS(t *testing.T) {
	got := FSFrom([]Options{{}})
	if got != nil {
		t.Error("expected nil when FS field is nil")
	}
}

func TestFSFrom_Set(t *testing.T) {
	fs := &FS{}
	got := FSFrom([]Options{{FS: fs}})
	if got != fs {
		t.Error("expected same FS pointer")
	}
}
