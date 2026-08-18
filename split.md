# FFmpeg flags explanation

Today 11:09 AM

#### You said:

Please explain all the flags used: func SplitStream(r io.ReadCloser) error { // 2. Set up ffmpeg to split on silence // This command looks for silence under -50dB lasting for 2 seconds cmd := exec.Command("ffmpeg", "-i", "pipe:0", "-f", "segment", "-segment\_time", "400", "-silence\_detect", "output\_%03d.mp3") cmd.Stdin = r cmd.Stdout = os.Stdout cmd.Stderr = os.Stderr err := cmd.Run() if err != nil { return fmt.Errorf("Command exited with an error: %w", err) } return nil }

Show moreShow less

  

#### ChatGPT said:

There are a few important issues with the command as written: **`-silence_detect` is not a valid FFmpeg option**, and `-segment_time 400` does not split on silence. It creates segments approximately every 400 seconds.

Here is what each argument does.

### `ffmpeg`

Runs FFmpeg.

ffmpeg

---

### `-i pipe:0`

Specifies the input.

\-i pipe:0

-   `-i` = input source
    
-   `pipe:0` = read the input from **stdin** (file descriptor 0)
    

Your Go code:

Go

cmd.Stdin \= r

connects `r` to FFmpeg's stdin.

So the data flow is:

io.ReadCloser

    ↓

Go cmd.Stdin

    ↓

FFmpeg stdin (pipe:0)

    ↓

FFmpeg decoder

This means you don't need to save the input to a temporary file first.

---

### `-f segment`

\-f segment

`-f` specifies the output **format/container/muxer**.

Here you're explicitly telling FFmpeg to use its **segment muxer**.

The segment muxer takes one input and produces multiple output files:

output\_000.mp3

output\_001.mp3

output\_002.mp3

...

This is different from encoding the entire input into one MP3.

---

### `-segment_time 400`

\-segment\_time 400

This tells the segment muxer to target segments of approximately **400 seconds**.

400 seconds = **6 minutes 40 seconds**.

So with a 30-minute input, you would approximately get:

output\_000.mp3   0:00 - 6:40

output\_001.mp3   6:40 - 13:20

output\_002.mp3   13:20 - 20:00

output\_003.mp3   20:00 - 26:40

output\_004.mp3   26:40 - 30:00

Importantly, **this has nothing to do with silence detection**.

---

### `-silence_detect`

\-silence\_detect

This is the problematic part.

**FFmpeg does not have a general `-silence_detect` command-line option.**

You probably want the **`silencedetect` audio filter**.

The syntax is:

\-af silencedetect=noise=-50dB:d=2

where:

-   `-af` = audio filter
    
-   `silencedetect` = FFmpeg's silence-detection filter
    
-   `noise=-50dB` = consider audio below `-50 dB` to be silence
    
-   `d=2` = silence must last at least 2 seconds
    

For example:

\-af silencedetect=noise=-50dB:d=2

However, there's another important distinction:

**`silencedetect` only detects and reports silence. It does not automatically tell the segment muxer to split at those points.**

So simply changing your command to:

ffmpeg -i pipe:0 -af silencedetect=noise=-50dB:d=2 -f segment -segment\_time 400 output\_%03d.mp3

will **detect silence**, but the segments will still be based on `-segment_time 400`.

---

### `output_%03d.mp3`

This is the output filename template.

output\_%03d.mp3

`%03d` is a C-style integer format:

-   `%d` = integer
    
-   `3` = minimum width of 3 digits
    
-   `0` = pad with zeros
    

Therefore:

output\_000.mp3

output\_001.mp3

output\_002.mp3

...

output\_010.mp3

output\_011.mp3

The segment muxer increments the number for each output file.

---

## What your command actually does

Ignoring the invalid `-silence_detect`, the intended command is essentially:

Read audio from stdin

        ↓

      FFmpeg

        ↓

Create MP3 segments

        ↓

Each approximately 400 seconds

        ↓

output\_000.mp3

output\_001.mp3

output\_002.mp3

...

It **does not currently split on silence**.

If your actual goal is:

> Split a continuous audio stream into roughly 400-second chunks, preferably breaking at a period of silence

then that's a different FFmpeg setup. `silencedetect` by itself isn't sufficient because it's an **analysis filter**, whereas `segment` needs an actual segmentation mechanism.

Also, there is a likely issue with the command's lack of an explicit audio codec/output configuration. Depending on the input, FFmpeg may choose defaults, but for predictable MP3 output I'd normally explicitly specify something like:

\-c:a libmp3lame -b:a 128k

So a simple **fixed-time** version would be:

ffmpeg -i pipe:0 -f segment -segment\_time 400 -c:a libmp3lame -b:a 128k output\_%03d.mp3

That gives you predictable 400-second MP3 files, but **not silence-aware splitting**.

Continue exploring:

-   Build silence-aware FFmpeg splitting
    
-   Explain FFmpeg filter versus muxer
    

  

#### You said:

Show a working FFmpeg and Go implementation that splits audio at detected silence while keeping segments near 400 seconds.

  

#### ChatGPT said:

The clean way to do this is **two-pass processing**:

1.  Run FFmpeg with `silencedetect` and collect all silence intervals.
    
2.  In Go, choose split points near 400 seconds that fall inside those silence intervals.
    
3.  Run FFmpeg a second time with `-segment_times` to create the actual segments.
    

This is preferable to trying to make `silencedetect` directly control the segment muxer: `silencedetect` detects and reports silence, while the segment muxer accepts explicit split times through `segment_times`. [![](https://www.google.com/s2/favicons?domain=https://ffmpeg.org&sz=128)FFmpeg+1](https://ffmpeg.org/ffmpeg-filters.html?utm_source=chatgpt.com)

## 1\. FFmpeg commands

### Pass 1: detect silence

Bash

ffmpeg \-i input.mp3 \\

  \-af "silencedetect=noise=-50dB:d=2" \\

  \-f null \-

This produces messages on stderr such as:

\[silencedetect @ ...\] silence\_start: 398.421

\[silencedetect @ ...\] silence\_end: 401.873 | silence\_duration: 3.452

\[silencedetect @ ...\] silence\_start: 801.124

\[silencedetect @ ...\] silence\_end: 804.002 | silence\_duration: 2.878

`noise=-50dB` means audio at or below that level is considered silence, and `d=2` requires at least two seconds of silence. [![](https://www.google.com/s2/favicons?domain=https://ffmpeg.org&sz=128)FFmpeg](https://ffmpeg.org/ffmpeg-filters.html?utm_source=chatgpt.com)

### Pass 2: split at the selected silence points

Suppose Go decides the best split points are:

401.873

804.002

1197.421

FFmpeg can then do:

Bash

ffmpeg \-i input.mp3 \\

  \-f segment \\

  \-segment\_times 401.873,804.002,1197.421 \\

  \-c:a libmp3lame \\

  \-b:a 128k \\

  output\_%03d.mp3

`segment_times` explicitly specifies the split timestamps. [![](https://www.google.com/s2/favicons?domain=https://ffmpeg.org&sz=128)FFmpeg](https://ffmpeg.org/ffmpeg-formats.html?utm_source=chatgpt.com)

---

# 2\. Go implementation

The following implementation takes your existing `io.ReadCloser`, writes it to a temporary file, performs the two FFmpeg passes, and produces segments near 400 seconds.

Go

package audio

  

import (

	"bufio"

	"fmt"

	"io"

	"os"

	"os/exec"

	"path/filepath"

	"regexp"

	"strconv"

	"strings"

)

  

type Silence struct {

	Start float64

	End   float64

}

  

const (

	targetSegmentDuration \= 400.0

	minSegmentDuration    \= 300.0

	maxSegmentDuration    \= 500.0

  

	silenceNoise     \= "-50dB"

	silenceDuration  \= "2"

)

  

func SplitStream(r io.ReadCloser, outputDir string) error {

	defer r.Close()

  

	// FFmpeg needs to process the input twice, so spool the stream

	// to a temporary file first.

	input, err := os.CreateTemp("", "audio-\*.input")

	if err != nil {

		return fmt.Errorf("create temporary input: %w", err)

## Silence detection

Go

var (

	silenceStartRE \= regexp.MustCompile(\`silence\_start:\\s\*(\[0-9.\]+)\`)

	silenceEndRE   \= regexp.MustCompile(\`silence\_end:\\s\*(\[0-9.\]+)\`)

)

  

func detectSilence(inputPath string) (\[\]Silence, error) {

	cmd := exec.Command(

		"ffmpeg",

		"-hide\_banner",

		"-i", inputPath,

		"-af", fmt.Sprintf(

			"silencedetect=noise=%s:d=%s",

			silenceNoise,

			silenceDuration,

		),

		"-f", "null",

		"-",

	)

  

	stderr, err := cmd.StderrPipe()

	if err != nil {

		return nil, fmt.Errorf("create stderr pipe: %w", err)

	}

  

	if err := cmd.Start(); err != nil {

		return nil, fmt.Errorf("start ffmpeg: %w", err)

	}

  

	var silences \[\]Silence

	var currentStart \*float64

  

	scanner := bufio.NewScanner(stderr)

  

	for scanner.Scan() {

		line := scanner.Text()

  

---

# 3\. Choosing the split points

This is the part that gives you the **"approximately 400 seconds, but preferably at silence"** behavior.

For example:

Audio timeline:

  

0                                                400

|------------------------------------------------|

                                      silence

                                   |------------|

                                   398        402

                                        ^

                                   split here

The algorithm looks for a silence interval around the desired 400-second boundary.

Go

func chooseSplitPoints(

	inputPath string,

	silences \[\]Silence,

) (\[\]float64, error) {

  

	duration, err := getDuration(inputPath)

	if err != nil {

		return nil, err

	}

  

	var splitTimes \[\]float64

  

	segmentStart := 0.0

  

	for {

		target := segmentStart + targetSegmentDuration

  

		if target \>= duration {

			break

		}

  

		silence, ok := findBestSilence(

			silences,

			target,

			segmentStart,

		)

  

		if !ok {

			// No suitable silence. Fall back to the target.

			splitTimes \= append(splitTimes, target)

			segmentStart \= target

			continue

		}

  

		// Split at the middle/end of the silence.

		split := silence.End

The actual silence-selection logic:

Go

func findBestSilence(

	silences \[\]Silence,

	target float64,

	segmentStart float64,

) (Silence, bool) {

  

	var best Silence

	bestDistance := float64(1 << 30)

  

	for \_, silence := range silences {

		// Don't create tiny segments.

		if silence.End\-segmentStart < minSegmentDuration {

			continue

		}

  

		// Don't let a segment grow beyond our maximum.

		if silence.End\-segmentStart \> maxSegmentDuration {

			continue

		}

  

		// The target needs to fall reasonably close to this silence.

		distance := abs(silence.End \- target)

  

		if distance < bestDistance {

			bestDistance \= distance

			best \= silence

		}

	}

  

	if bestDistance \== float64(1<<30) {

		return Silence{}, false

	}

  

	return best, true

}

  

With these values:

Target:       400 seconds

Minimum:      300 seconds

Maximum:      500 seconds

you'll generally get segments like:

0:00  → 6:38

6:38  → 13:27

13:27 → 19:54

19:54 → 26:42

rather than blindly cutting at:

6:40

13:20

20:00

26:40

---

# 4\. Get the input duration

Go

func getDuration(inputPath string) (float64, error) {

	cmd := exec.Command(

		"ffprobe",

		"-v", "error",

		"-show\_entries", "format=duration",

		"-of", "default=noprint\_wrappers=1:nokey=1",

		inputPath,

	)

  

	output, err := cmd.Output()

	if err != nil {

		return 0, fmt.Errorf("ffprobe: %w", err)

	}

  

	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)

	if err != nil {

		return 0, fmt.Errorf("parse duration %q: %w", output, err)

	}

  

	return duration, nil

}

---

# 5\. Perform the actual split

Go

func splitAtTimes(

	inputPath string,

	outputDir string,

	splitTimes \[\]float64,

) error {

  

	if err := os.MkdirAll(outputDir, 0755); err != nil {

		return fmt.Errorf("create output directory: %w", err)

	}

  

	times := make(\[\]string, len(splitTimes))

  

	for i, t := range splitTimes {

		times\[i\] \= strconv.FormatFloat(t, 'f', 3, 64)

	}

  

	outputPattern := filepath.Join(

		outputDir,

		"output\_%03d.mp3",

	)

  

	args := \[\]string{

		"-hide\_banner",

  

		"-i", inputPath,

  

		"-map", "0:a:0",

  

		"-f", "segment",

  

		"-segment\_times", strings.Join(times, ","),

  

		"-c:a", "libmp3lame",

		"-b:a", "128k",

  

		outputPattern,

## One correction I'd make to the algorithm

I would actually **split at the middle of the silence**, rather than `silence.End`.

For example:

speech              silence                 speech

████████████████████░░░░░░░░░░░░░░░░░░░░░░░████████

                    398                  403

                         ^

                      split

Using:

Go

split := (silence.Start + silence.End) / 2

means neither segment gets the entire silence.

Change:

Go

split := silence.End

to:

Go

split := (silence.Start + silence.End) / 2

That generally produces cleaner boundaries.

---

## Important limitation with a live stream

Your original function takes:

Go

func SplitStream(r io.ReadCloser) error

If `r` is a **live/infinite stream**, the two-pass approach above cannot work directly because FFmpeg needs to see the entire stream before the second pass can start.

For a live audio stream, I'd instead implement a **rolling segmentation process**:

             live stream

                  │

                  ▼

          ┌───────────────┐

          │    FFmpeg     │

          │ decode/audio  │

          └───────┬───────┘

                  │

                  ▼

          Go silence detector

                  │

                  ▼

        approaching 400 seconds?

              /          \\

            no            yes

            │              │

            │       wait for silence

            │              │

            │              ▼

            │        silence found

            │              │

            │              ▼

            └──────► write segment

                           │

                           ▼

                     next segment

For your use case, if this is the **MP3 streamer/recorder you mentioned**, that rolling approach is probably the better architecture. It avoids writing the entire stream to disk and lets you close a segment when a silence occurs after approximately 400 seconds.

Continue exploring:

-   Implement rolling live-stream segmentation
    
-   Harden the two-pass splitter
    

![](https://www.google.com/s2/favicons?domain=https://ffmpeg.org&sz=32)

Sources

ChatGPT can make mistakes. Check important info.

  

Think
