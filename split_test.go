package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitStream(t *testing.T) {
	err := SplitStream(t.Context(), "/home/wizardofmath/go/src/github.com/hibooboo2/gradio/recordings/gayphx-2026-06-06_13-52-37.mp3")
	require.NoError(t, err)
}
