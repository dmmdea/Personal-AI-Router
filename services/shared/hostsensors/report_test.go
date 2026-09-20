// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hostsensors

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 19, 21, 40, 0, 0, time.UTC)
	in := Report{
		HelperVersion: "0.1.0",
		CPU:           &CPUReading{PackageCelsius: 58, TjMaxCelsius: 110, Source: "intel-msr", SampledAt: at},
	}
	var buf bytes.Buffer
	if err := Encode(&buf, in); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Fatalf("Encode must newline-terminate: %q", buf.String())
	}
	out, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Schema != Schema {
		t.Fatalf("schema = %d, want %d", out.Schema, Schema)
	}
	if out.CPU == nil || *out.CPU != *in.CPU {
		t.Fatalf("cpu = %+v, want %+v", out.CPU, in.CPU)
	}
	if out.HelperVersion != "0.1.0" {
		t.Fatalf("helper_version = %q", out.HelperVersion)
	}
}

func TestDecodeRefusesNewerSchema(t *testing.T) {
	_, err := Decode(strings.NewReader(`{"schema":99,"cpu":{"package_celsius":40}}`))
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("err = %v, want ErrSchema", err)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode(strings.NewReader("not json")); err == nil {
		t.Fatal("expected an error for non-JSON input")
	}
}

func TestDecodeHelperWithoutSensor(t *testing.T) {
	r, err := Decode(strings.NewReader(`{"schema":1,"helper_version":"0.1.0","error":"PawnIO is not installed"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.CPU != nil {
		t.Fatalf("cpu = %+v, want nil", r.CPU)
	}
	if _, ok := r.CPUPackage(time.Now(), time.Minute); ok {
		t.Fatal("a report without a CPU reading must not yield a temperature")
	}
}

// TestCPUPackageFreshness pins the freshness rule: a reading counts only while
// it is non-zero and no older than the window; a frozen or future-dated
// sample is dropped rather than repeated.
func TestCPUPackageFreshness(t *testing.T) {
	now := time.Date(2026, 9, 19, 21, 40, 30, 0, time.UTC)
	window := 30 * time.Second
	cases := []struct {
		name    string
		reading *CPUReading
		want    uint32
		ok      bool
	}{
		{"fresh", &CPUReading{PackageCelsius: 58, SampledAt: now.Add(-5 * time.Second)}, 58, true},
		{"at the edge", &CPUReading{PackageCelsius: 58, SampledAt: now.Add(-window)}, 58, true},
		{"stale", &CPUReading{PackageCelsius: 58, SampledAt: now.Add(-window - time.Second)}, 0, false},
		{"zero reading", &CPUReading{PackageCelsius: 0, SampledAt: now}, 0, false},
		{"unstamped", &CPUReading{PackageCelsius: 58}, 0, false},
		{"far future", &CPUReading{PackageCelsius: 58, SampledAt: now.Add(2 * window)}, 0, false},
		{"no cpu", nil, 0, false},
	}
	for _, c := range cases {
		got, ok := Report{CPU: c.reading}.CPUPackage(now, window)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: CPUPackage = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestDecodeBoundsTheRead(t *testing.T) {
	huge := `{"schema":1,"error":"` + strings.Repeat("x", MaxReportBytes) + `"}`
	if _, err := Decode(strings.NewReader(huge)); err == nil {
		t.Fatal("a report past MaxReportBytes must fail to decode, not be read whole")
	}
}

// TestCPUPackageWattsFreshness applies the same window CPUPackage does, and
// pins that the two readings are asked for independently: a helper on a part
// whose energy counter it could not resolve publishes a temperature and no
// wattage, and neither absence may take the other down.
func TestCPUPackageWattsFreshness(t *testing.T) {
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	fresh := Report{CPU: &CPUReading{PackageCelsius: 55, PackageWatts: 140, SampledAt: now.Add(-2 * time.Second)}}
	if w, ok := fresh.CPUPackageWatts(now, 30*time.Second); !ok || w != 140 {
		t.Fatalf("fresh: %v, %v; want 140, true", w, ok)
	}

	stale := Report{CPU: &CPUReading{PackageCelsius: 55, PackageWatts: 140, SampledAt: now.Add(-time.Hour)}}
	if _, ok := stale.CPUPackageWatts(now, 30*time.Second); ok {
		t.Error("a stale report published a wattage")
	}
	if _, ok := stale.CPUPackage(now, 30*time.Second); ok {
		t.Error("a stale report published a temperature")
	}

	// Temperature without power: the CPU section is still fully usable.
	tempOnly := Report{CPU: &CPUReading{PackageCelsius: 55, SampledAt: now}}
	if _, ok := tempOnly.CPUPackageWatts(now, 30*time.Second); ok {
		t.Error("a report with no wattage published one")
	}
	if c, ok := tempOnly.CPUPackage(now, 30*time.Second); !ok || c != 55 {
		t.Errorf("temperature = %d, %v; want 55, true", c, ok)
	}

	if _, ok := (Report{}).CPUPackageWatts(now, 30*time.Second); ok {
		t.Error("a report with no CPU section published a wattage")
	}
	// A future stamp is refused exactly as it is for the temperature.
	ahead := Report{CPU: &CPUReading{PackageWatts: 140, SampledAt: now.Add(time.Hour)}}
	if _, ok := ahead.CPUPackageWatts(now, 30*time.Second); ok {
		t.Error("a report stamped in the future published a wattage")
	}
}

// TestPackageWattsIsAdditiveOnTheWire: the field was added inside an existing
// section, so an unmetered report must serialize exactly as it did before and
// Schema must not move — a bumped schema would make every already-installed
// reader refuse a report whose temperature it could still decode perfectly.
func TestPackageWattsIsAdditiveOnTheWire(t *testing.T) {
	if Schema != 1 {
		t.Fatalf("Schema = %d; adding an optional field must not bump it", Schema)
	}

	var unmetered bytes.Buffer
	if err := Encode(&unmetered, Report{CPU: &CPUReading{PackageCelsius: 55, Source: "intel-msr"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unmetered.String(), "package_watts") {
		t.Fatalf("an unmetered report carries package_watts: %s", unmetered.String())
	}

	var metered bytes.Buffer
	if err := Encode(&metered, Report{CPU: &CPUReading{PackageCelsius: 55, PackageWatts: 140, Source: "intel-msr"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metered.String(), `"package_watts":140`) {
		t.Fatalf("metered report missing package_watts: %s", metered.String())
	}

	back, err := Decode(&metered)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.CPU == nil || back.CPU.PackageWatts != 140 {
		t.Fatalf("PackageWatts did not survive a round trip: %+v", back.CPU)
	}
}
