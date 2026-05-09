package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const version = "1.0.0"

// findMusicFiles searches for audio files in the given directory
func findMusicFiles(dir string) ([]string, error) {
	supportedFormats := []string{".mp3", ".flac", ".ogg", ".wav", ".m4a", ".aac"}
	var files []string

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		for _, format := range supportedFormats {
			if ext == format {
				files = append(files, path)
				break
			}
		}
		return nil
	})

	return files, err
}

// playWithMpv launches mpv with the provided files or directory
func playWithMpv(args []string, shuffle bool, loop bool, volume int) error {
	mpvArgs := []string{}

	if shuffle {
		mpvArgs = append(mpvArgs, "--shuffle")
	}
	if loop {
		mpvArgs = append(mpvArgs, "--loop-playlist=inf")
	}
	if volume >= 0 && volume <= 100 {
		mpvArgs = append(mpvArgs, fmt.Sprintf("--volume=%d", volume))
	}

	// Enable terminal UI and audio-only mode
	// --no-audio-display prevents album art windows from popping up
	mpvArgs = append(mpvArgs, "--no-video", "--no-audio-display", "--term-osd-bar")
	mpvArgs = append(mpvArgs, args...)

	cmd := exec.Command("mpv", mpvArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func main() {
	var shuffle bool
	var loop bool
	var volume int

	rootCmd := &cobra.Command{
		Use:     "cliamp [file/directory...]",
		Short:   "cliamp — a minimal CLI music player powered by mpv",
		Version: version,
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var playArgs []string

			for _, arg := range args {
				info, err := os.Stat(arg)
				if err != nil {
					return fmt.Errorf("cannot access %q: %w", arg, err)
				}

				if info.IsDir() {
					files, err := findMusicFiles(arg)
					if err != nil {
						return fmt.Errorf("error scanning directory %q: %w", arg, err)
					}
					if len(files) == 0 {
						return fmt.Errorf("no supported audio files found in %q", arg)
					}
					playArgs = append(playArgs, files...)
				} else {
					playArgs = append(playArgs, arg)
				}
			}

			if _, err := exec.LookPath("mpv"); err != nil {
				return fmt.Errorf("mpv is not installed or not in PATH")
			}

			return playWithMpv(playArgs, shuffle, loop, volume)
		},
	}

	rootCmd.Flags().BoolVarP(&shuffle, "shuffle", "s", false, "Shuffle the playlist")
	rootCmd.Flags().BoolVarP(&loop, "loop", "l", false, "Loop the playlist indefinitely")
	// Default volume lowered to 80 to avoid unexpectedly loud playback
	rootCmd.Flags().IntVarP(&volume, "volume", "v", 80, "Set playback volume (0-100)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
