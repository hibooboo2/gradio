package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

func SplitStream(r io.Reader) error {
	// ffmpeg -i gayphx-2026-08-15_11-25-11.mp3 -af "silencedetect=noise=-22dB:d=1" -f null -

	// 2. Set up ffmpeg to split on silence
	// This command looks for silence under -22dB lasting for 1 seconds
	cmd := exec.Command("ffmpeg", "-i", "pipe:0",
		"-af", "silencedetect=noise=-22dB:d=1", "-f", "null", "-")
	// "output_%03d.mp3")

	cmd.Stdin = r
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("Command exited with an error: %w", err)
	}

	return nil
}
