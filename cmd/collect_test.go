package cmd

import (
	"testing"
)

func TestRunDocker(t *testing.T) {
	files, err := runDocker()
	if err != nil {
		t.Errorf("Error: %v", err)
	}
	if files == nil {
		t.Errorf("Error: files is nil")
	}
	t.Log("Files: %v", files)
}
