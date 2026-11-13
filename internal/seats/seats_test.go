package seats

import "testing"

func TestAcquireHonorsCapAndRelease(t *testing.T) {
	table := New(map[string]int{"glm/glm-5.3": 1})
	release, ok := table.Acquire("glm", "glm-5.3")
	if !ok {
		t.Fatal("first acquire")
	}
	if _, ok := table.Acquire("glm", "glm-5.3"); ok {
		t.Fatal("second acquire should wait")
	}
	release()
	if _, ok := table.Acquire("glm", "glm-5.3"); !ok {
		t.Fatal("acquire after release")
	}
}

func TestUnlimitedWhenUncapped(t *testing.T) {
	table := New(nil)
	if _, ok := table.Acquire("glm", "glm-5.3"); !ok {
		t.Fatal("uncapped provider is unlimited")
	}
}
