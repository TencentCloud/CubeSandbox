package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	sparkChars = "▁▂▃▄▅▆▇█"
	barFull    = '█'
	barChars   = "▏▎▍▌▋▊▉█"
)

func Percentile(data []float64, p float64) float64 {
	if len(data) == 0 {
		return math.NaN()
	}
	s := make([]float64, len(data))
	copy(s, data)
	sort.Float64s(s)
	k := int(math.Ceil(float64(len(s))*p/100.0)) - 1
	if k < 0 {
		k = 0
	}
	if k >= len(s) {
		k = len(s) - 1
	}
	return s[k]
}

func Mean(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range data {
		sum += v
	}
	return sum / float64(len(data))
}

func StdDev(data []float64) float64 {
	if len(data) < 2 {
		return 0
	}
	m := Mean(data)
	sum := 0.0
	for _, v := range data {
		d := v - m
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(data)-1))
}

func Min(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	m := data[0]
	for _, v := range data[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func Max(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	m := data[0]
	for _, v := range data[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func Sparkline(values []float64, width int) string {
	if len(values) == 0 {
		return ""
	}
	runes := []rune(sparkChars)

	display := values
	if len(values) > width {
		chunk := float64(len(values)) / float64(width)
		display = make([]float64, width)
		for i := 0; i < width; i++ {
			lo := int(float64(i) * chunk)
			hi := int(float64(i+1) * chunk)
			if hi > len(values) {
				hi = len(values)
			}
			if hi <= lo {
				continue
			}
			display[i] = Mean(values[lo:hi])
		}
	}

	lo, hi := Min(display), Max(display)
	spread := hi - lo
	if spread == 0 {
		spread = 1
	}

	var b strings.Builder
	for _, v := range display {
		idx := int((v - lo) / spread * 7)
		if idx > 7 {
			idx = 7
		}
		b.WriteRune(runes[idx])
	}
	return b.String()
}

type HistBucket struct {
	Lo    float64
	Hi    float64
	Count int
}

func Histogram(data []float64, numBuckets int) []HistBucket {
	if len(data) == 0 {
		return nil
	}
	lo, hi := Min(data), Max(data)
	if hi == lo {
		hi = lo + 1
	}
	w := (hi - lo) / float64(numBuckets)
	buckets := make([]HistBucket, numBuckets)
	for i := range buckets {
		buckets[i].Lo = lo + float64(i)*w
		buckets[i].Hi = buckets[i].Lo + w
	}
	for _, v := range data {
		idx := int((v - lo) / w)
		if idx >= numBuckets {
			idx = numBuckets - 1
		}
		buckets[idx].Count++
	}
	return buckets
}

func HistogramBar(count, maxCount, width int) string {
	if maxCount == 0 {
		return ""
	}
	runes := []rune(barChars)
	full := float64(count) / float64(maxCount) * float64(width)
	whole := int(full)
	frac := full - float64(whole)

	var b strings.Builder
	for i := 0; i < whole; i++ {
		b.WriteRune(runes[7])
	}
	if frac > 0 && whole < width {
		idx := int(frac * 8)
		if idx > 7 {
			idx = 7
		}
		b.WriteRune(runes[idx])
	}
	return b.String()
}

func FormatMs(ms float64) string {
	return fmt.Sprintf("%.1f", ms)
}
