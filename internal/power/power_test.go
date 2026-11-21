package power

import (
	"context"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

func ps(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v + "\n")}
	}
	return m
}

func TestReadPowerSupply(t *testing.T) {
	ac := core.PowerState{Known: true, OnAC: true}
	tests := []struct {
		name    string
		fsys    fstest.MapFS
		want    core.PowerState
		wantErr bool
	}{
		{"desktop with no supplies", fstest.MapFS{}, ac, false},
		{"desktop with only mains", ps(map[string]string{"AC/type": "Mains", "AC/online": "1"}), ac, false},
		{"desktop with a wireless mouse battery", ps(map[string]string{
			"hidpp_battery_0/type": "Battery", "hidpp_battery_0/scope": "Device", "hidpp_battery_0/capacity": "5",
		}), ac, false},
		{"laptop plugged in", ps(map[string]string{
			"AC/type": "Mains", "AC/online": "1",
			"BAT0/type": "Battery", "BAT0/status": "Charging", "BAT0/capacity": "62",
		}), core.PowerState{Known: true, OnAC: true, HasBattery: true, BatteryPercent: 62}, false},
		{"laptop on battery", ps(map[string]string{
			"ADP1/type": "Mains", "ADP1/online": "0",
			"BAT1/type": "Battery", "BAT1/status": "Discharging", "BAT1/capacity": "31",
		}), core.PowerState{Known: true, HasBattery: true, BatteryPercent: 31}, false},
		{"laptop charging over USB-C", ps(map[string]string{
			"ADP1/type": "Mains", "ADP1/online": "0",
			"ucsi-source-psy-USBC000:001/type": "USB", "ucsi-source-psy-USBC000:001/online": "1",
			"BAT0/type": "Battery", "BAT0/capacity": "80",
		}), core.PowerState{Known: true, OnAC: true, HasBattery: true, BatteryPercent: 80}, false},
		{"ThinkPad with two batteries weighted by energy", ps(map[string]string{
			"AC/type": "Mains", "AC/online": "0",
			"BAT0/type": "Battery", "BAT0/capacity": "90", "BAT0/energy_now": "21600000", "BAT0/energy_full": "24000000",
			"BAT1/type": "Battery", "BAT1/capacity": "10", "BAT1/energy_now": "7200000", "BAT1/energy_full": "72000000",
		}), core.PowerState{Known: true, HasBattery: true, BatteryPercent: 30}, false},
		{"two batteries without energy files average capacity", ps(map[string]string{
			"AC/type": "Mains", "AC/online": "0",
			"BAT0/type": "Battery", "BAT0/capacity": "90",
			"BAT1/type": "Battery", "BAT1/capacity": "21",
		}), core.PowerState{Known: true, HasBattery: true, BatteryPercent: 56}, false},
		{"battery with charge files", ps(map[string]string{
			"AC/type": "Mains", "AC/online": "0",
			"BAT0/type": "Battery", "BAT0/charge_now": "2500000", "BAT0/charge_full": "5000000",
		}), core.PowerState{Known: true, HasBattery: true, BatteryPercent: 50}, false},
		{"no mains entry and discharging", ps(map[string]string{
			"battery/type": "Battery", "battery/status": "Discharging", "battery/capacity": "44",
		}), core.PowerState{Known: true, HasBattery: true, BatteryPercent: 44}, false},
		{"no mains entry and full", ps(map[string]string{
			"battery/type": "Battery", "battery/status": "Full", "battery/capacity": "100",
		}), core.PowerState{Known: true, OnAC: true, HasBattery: true, BatteryPercent: 100}, false},
		{"battery with no capacity", ps(map[string]string{
			"AC/type": "Mains", "AC/online": "0", "BAT0/type": "Battery",
		}), core.PowerState{}, true},
		{"battery with garbage capacity", ps(map[string]string{
			"AC/type": "Mains", "AC/online": "0", "BAT0/type": "Battery", "BAT0/capacity": "lots",
		}), core.PowerState{}, true},
		// A supply with no type might be the machine's battery: unknown,
		// not AC (review r1 partB sol-F6).
		{"supply with no type", ps(map[string]string{
			"BAT0/status": "Discharging", "BAT0/capacity": "12",
		}), core.PowerState{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sysfsPower{fsys: tt.fsys}.Power(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// failFS fails reads of one file with an I/O error.
type failFS struct {
	fstest.MapFS
	bad string
}

func (f failFS) Open(name string) (fs.File, error) {
	if name == f.bad {
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EIO}
	}
	return f.MapFS.Open(name)
}

func (f failFS) ReadFile(name string) ([]byte, error) {
	if name == f.bad {
		return nil, &fs.PathError{Op: "read", Path: name, Err: syscall.EIO}
	}
	return f.MapFS.ReadFile(name)
}

// A battery whose type or status can't be read leaves power unknown
// instead of reporting AC (review r1 partB sol-F6).
func TestUnreadableSupplyFailsClosed(t *testing.T) {
	laptop := ps(map[string]string{"BAT0/type": "Battery", "BAT0/status": "Discharging", "BAT0/capacity": "12"})
	for _, bad := range []string{"BAT0/type", "BAT0/status"} {
		got, err := sysfsPower{fsys: failFS{laptop, bad}}.Power(context.Background())
		if err == nil || got.Known {
			t.Errorf("%s unreadable: got %+v, err %v", bad, got, err)
		}
	}
}

// A peripheral battery (scope=Device) that is asleep returns EIO or ENODATA
// on read. It is not the machine's own, so it is skipped and the machine's
// own supplies decide (review r2 partB first-F1). An unscoped or System
// supply with the same error still fails closed.
func TestAsleepDeviceSupplyIsSkipped(t *testing.T) {
	mouse := map[string]string{
		"hidpp_battery_0/type": "Battery", "hidpp_battery_0/scope": "Device",
		"hidpp_battery_0/status": "Discharging", "hidpp_battery_0/capacity": "5",
		"hidpp_battery_0/online": "1",
	}
	desktop := ps(mouse)
	laptopFiles := map[string]string{
		"AC/type": "Mains", "AC/online": "1",
		"BAT0/type": "Battery", "BAT0/scope": "System", "BAT0/status": "Charging", "BAT0/capacity": "62",
	}
	for k, v := range mouse {
		laptopFiles[k] = v
	}
	laptop := ps(laptopFiles)
	for _, bad := range []string{"hidpp_battery_0/capacity", "hidpp_battery_0/status", "hidpp_battery_0/online"} {
		got, err := sysfsPower{fsys: failFS{desktop, bad}}.Power(context.Background())
		if err != nil || got != (core.PowerState{Known: true, OnAC: true}) {
			t.Errorf("desktop, %s unreadable: got %+v, err %v", bad, got, err)
		}
		got, err = sysfsPower{fsys: failFS{laptop, bad}}.Power(context.Background())
		want := core.PowerState{Known: true, OnAC: true, HasBattery: true, BatteryPercent: 62}
		if err != nil || got != want {
			t.Errorf("laptop, %s unreadable: got %+v, err %v", bad, got, err)
		}
	}
	for _, bad := range []string{"BAT0/capacity", "BAT0/scope"} {
		got, err := sysfsPower{fsys: failFS{laptop, bad}}.Power(context.Background())
		if err == nil || got.Known {
			t.Errorf("laptop, %s unreadable: got %+v, err %v", bad, got, err)
		}
	}
}

const (
	pmsetMacBookBattery = "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=4653155)\t31%; discharging; 4:12 remaining present: true\n"
	pmsetMacBookAC      = "Now drawing from 'AC Power'\n -InternalBattery-0 (id=36700259)\t100%; charged; 0:00 remaining present: true\n"
	pmsetMacBookCharge  = "Now drawing from 'AC Power'\n -InternalBattery-0 (id=36700259)\t7%; charging; (no estimate) present: true\n"
	pmsetMacMini        = "Now drawing from 'AC Power'\n"
	pmsetUPS            = "Now drawing from 'UPS Power'\n -CP1500PFCLCD (id=5)\t87%; discharging; 0:21 remaining present: true\n"
)

func TestParsePmset(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    core.PowerState
		wantErr bool
	}{
		{"MacBook on battery", pmsetMacBookBattery, core.PowerState{Known: true, HasBattery: true, BatteryPercent: 31}, false},
		{"MacBook on AC charged", pmsetMacBookAC, core.PowerState{Known: true, OnAC: true, HasBattery: true, BatteryPercent: 100}, false},
		{"MacBook on AC charging low", pmsetMacBookCharge, core.PowerState{Known: true, OnAC: true, HasBattery: true, BatteryPercent: 7}, false},
		{"Mac mini with no battery", pmsetMacMini, core.PowerState{Known: true, OnAC: true}, false},
		{"desktop on a UPS during an outage", pmsetUPS, core.PowerState{Known: true, HasBattery: true, BatteryPercent: 87}, false},
		{"empty output", "", core.PowerState{}, true},
		{"garbage", "pmset: command failed\n", core.PowerState{}, true},
		{"battery power with no percent", "Now drawing from 'Battery Power'\n", core.PowerState{}, true},
		{"unknown source", "Now drawing from 'Solar Power'\n", core.PowerState{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePmset(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func canned(outs map[string]string, errs map[string]error, calls *[]string) runner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if calls != nil {
			*calls = append(*calls, key)
		}
		if err, ok := errs[key]; ok {
			return nil, err
		}
		out, ok := outs[key]
		if !ok {
			return nil, errors.New("unexpected command " + key)
		}
		return []byte(out), nil
	}
}

func TestPmsetPowerRunsPmset(t *testing.T) {
	var calls []string
	p := pmsetPower{run: canned(map[string]string{"/usr/bin/pmset -g batt": pmsetMacBookBattery}, nil, &calls)}
	got, err := p.Power(context.Background())
	if err != nil || got.BatteryPercent != 31 || got.OnAC {
		t.Fatalf("got %+v, %v", got, err)
	}
	p = pmsetPower{run: canned(nil, map[string]error{"/usr/bin/pmset -g batt": errors.New("timed out")}, nil)}
	if _, err := p.Power(context.Background()); err == nil {
		t.Fatal("want error when pmset fails")
	}
}

const ioregSample = `+-o Root  <class IORegistryEntry, id 0x100000100, retain 34>
  +-o J316sAP  <class IOPlatformExpertDevice, id 0x100000202, registered, matched, active, busy 0 (3 ms), retain 41>
    +-o IOResources  <class IOResources, id 0x100000101, registered, matched, active, busy 0 (0 ms), retain 290>
      +-o IOHIDSystem  <class IOHIDSystem, id 0x1000004c5, registered, matched, active, busy 0 (0 ms), retain 29>
        | {
        |   "HIDIdleTime" = 3635190916
        |   "IOClass" = "IOHIDSystem"
        | }
`

func TestParseIoregIdle(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"one entry", ioregSample, 3635190916 * time.Nanosecond, false},
		{"two entries takes the smallest", ioregSample + `| "HIDIdleTime" = 1000` + "\n", 1000, false},
		{"missing", "+-o Root\n", 0, true},
		{"empty", "", 0, true},
		{"overflow", `"HIDIdleTime" = 99999999999999999999999`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIoregIdle(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got %v, %v; want %v, err %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
	i := ioregIdle{run: canned(map[string]string{"/usr/sbin/ioreg -c IOHIDSystem": ioregSample}, nil, nil)}
	if d, err := i.IdleFor(context.Background()); err != nil || d != 3635190916 {
		t.Fatalf("IdleFor = %v, %v", d, err)
	}
}

func TestLoginctlIdle(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	since := now.Add(-12 * time.Minute).UnixMicro()
	seat := "loginctl show-seat seat0 -p ActiveSession --value"
	sess := "loginctl show-session 3 -p IdleHint -p IdleSinceHint"
	tests := []struct {
		name    string
		outs    map[string]string
		errs    map[string]error
		want    time.Duration
		wantErr bool
	}{
		{"idle 12 minutes", map[string]string{seat: "3\n", sess: "IdleHint=yes\nIdleSinceHint=" + itoa(since) + "\n"}, nil, 12 * time.Minute, false},
		{"active", map[string]string{seat: "3\n", sess: "IdleHint=no\nIdleSinceHint=0\n"}, nil, 0, false},
		{"idle since the future", map[string]string{seat: "3\n", sess: "IdleHint=yes\nIdleSinceHint=" + itoa(now.Add(time.Hour).UnixMicro()) + "\n"}, nil, 0, false},
		{"no active session (headless)", map[string]string{seat: "\n"}, nil, 0, true},
		{"odd session id", map[string]string{seat: "3; rm -rf /\n"}, nil, 0, true},
		{"loginctl missing", nil, map[string]error{seat: errors.New("exec: \"loginctl\": executable file not found")}, 0, true},
		{"show-session fails", map[string]string{seat: "3\n"}, map[string]error{sess: errors.New("exit status 1")}, 0, true},
		{"malformed", map[string]string{seat: "3\n", sess: "garbage\n"}, nil, 0, true},
		{"bad hint", map[string]string{seat: "3\n", sess: "IdleHint=maybe\nIdleSinceHint=1\n"}, nil, 0, true},
		{"no since", map[string]string{seat: "3\n", sess: "IdleHint=yes\n"}, nil, 0, true},
		{"bad since", map[string]string{seat: "3\n", sess: "IdleHint=yes\nIdleSinceHint=soon\n"}, nil, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := loginctlIdle{run: canned(tt.outs, tt.errs, nil), now: func() time.Time { return now }}
			got, err := l.IdleFor(context.Background())
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got %v, %v; want %v, err %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

const meminfoSample = `MemTotal:       32562096 kB
MemFree:         1203456 kB
MemAvailable:   20480000 kB
Buffers:          512000 kB
Cached:         18000000 kB
`

func TestParseMeminfo(t *testing.T) {
	if n, err := parseMeminfo(meminfoSample, "MemTotal"); err != nil || n != 32562096*1024 {
		t.Fatalf("MemTotal = %d, %v", n, err)
	}
	if n, err := parseMeminfo(meminfoSample, "MemAvailable"); err != nil || n != 20480000*1024 {
		t.Fatalf("MemAvailable = %d, %v", n, err)
	}
	for _, bad := range []string{"", "MemFree: 1 kB\n", "MemTotal: lots kB\n", "MemTotal:\n", "MemTotal: -5 kB\n"} {
		if _, err := parseMeminfo(bad, "MemTotal"); err == nil {
			t.Errorf("parseMeminfo(%q) = nil error", bad)
		}
	}
}

func TestDarwinTotalMemory(t *testing.T) {
	run := canned(map[string]string{"/usr/sbin/sysctl -n hw.memsize": "34359738368\n"}, nil, nil)
	if n, err := darwinTotalMemory(context.Background(), run); err != nil || n != 34359738368 {
		t.Fatalf("got %d, %v", n, err)
	}
	for _, bad := range []string{"", "abc", "0", "-1"} {
		run := canned(map[string]string{"/usr/sbin/sysctl -n hw.memsize": bad}, nil, nil)
		if _, err := darwinTotalMemory(context.Background(), run); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestFromSystemPowerStatus(t *testing.T) {
	tests := []struct {
		name          string
		ac, flag, pct byte
		want          core.PowerState
		wantErr       bool
	}{
		{"desktop, no system battery", 1, 128, 255, core.PowerState{Known: true, OnAC: true}, false},
		{"desktop, no battery, line unknown", 255, 128, 255, core.PowerState{Known: true, OnAC: true}, false},
		{"laptop plugged in charging", 1, 8, 64, core.PowerState{Known: true, OnAC: true, HasBattery: true, BatteryPercent: 64}, false},
		{"laptop on battery", 0, 2, 31, core.PowerState{Known: true, HasBattery: true, BatteryPercent: 31}, false},
		{"laptop on battery critical", 0, 4, 3, core.PowerState{Known: true, HasBattery: true, BatteryPercent: 3}, false},
		{"plugged in, percent unknown", 1, 255, 255, core.PowerState{Known: true, OnAC: true, HasBattery: true}, false},
		{"on battery, percent unknown", 0, 1, 255, core.PowerState{}, true},
		{"line unknown with a battery", 255, 1, 50, core.PowerState{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fromSystemPowerStatus(tt.ac, tt.flag, tt.pct)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v, err %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestFakes(t *testing.T) {
	f := Fake{State: core.PowerState{Known: true, OnAC: true}}
	if s, err := f.Power(context.Background()); err != nil || !s.OnAC {
		t.Fatal("Fake")
	}
	i := FakeIdle{Err: errors.New("x")}
	if _, err := i.IdleFor(context.Background()); err == nil {
		t.Fatal("FakeIdle")
	}
}

func TestCPUs(t *testing.T) {
	if CPUs() < 1 {
		t.Fatal("CPUs < 1")
	}
}
