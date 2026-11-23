package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/budget"
	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// Wake-on-LAN timing.
const (
	// wakeEvery: at most one wake per device this often, from any one
	// decider and from any one sender.
	wakeEvery = 10 * time.Minute
	// wakeWait: a woken device that doesn't answer its hello by then
	// counts as a timeout, and its next wake backs off.
	wakeWait = 3 * time.Minute
	// wakeBackoffMax caps the backoff after misses in a row.
	wakeBackoffMax = 4 * time.Hour
	// wakeRecent: a device not seen this long is not woken.
	wakeRecent = 7 * 24 * time.Hour
	// knownKeep: a device not seen this long leaves the known table.
	knownKeep = 30 * 24 * time.Hour
	// awakeWithin: a device whose hello answered this recently is awake.
	// Discovery runs every 30 s, so this is two and a half rounds.
	awakeWithin = 75 * time.Second
	// wakeTickEvery is how often a broker device looks for jobs to wake a
	// device for.
	wakeTickEvery = 5 * time.Second
	// knownSaveEvery: a known device's last-seen time alone rewrites the
	// file at most this often.
	knownSaveEvery = 10 * time.Minute
	// wakeInfoTimeout bounds one read of this device's wake facts.
	wakeInfoTimeout = 5 * time.Second
	// maxWakeSenders is how many same-LAN peers a decider asks in turn.
	maxWakeSenders = 3
)

// Wake results, as status and the tentmaker_wakes_total counter name them.
// A wake is first sent or failed; a sent one later turns online or
// timeout.
const (
	WakeSent    = "sent"
	WakeFailed  = "failed"
	WakeOnline  = "online"
	WakeTimeout = "timeout"
)

// WakeResults lists every result, in the order metrics report them.
var WakeResults = []string{WakeSent, WakeFailed, WakeOnline, WakeTimeout}

// WakeInfo is what a device says about waking it, recorded at join and on
// each heartbeat and sent in its /v1/hello.
type WakeInfo struct {
	LAN      LAN    `json:"lan"`
	LANError string `json:"lan_error,omitempty"`
	// OptOut is mesh.wake = false in its config.
	OptOut bool `json:"opt_out,omitempty"`
	// DoNotDisturb: its owner paused CI on it (tentmaker pause).
	DoNotDisturb bool `json:"do_not_disturb,omitempty"`
	// Power is its power state; nil when it couldn't be read.
	Power *core.PowerState `json:"power,omitempty"`
	// Labels and Platforms are what its worker asks brokers for.
	Labels    []string        `json:"labels,omitempty"`
	Platforms []core.Platform `json:"platforms,omitempty"`
	// Slots is how many jobs its budget fits when it is plugged in.
	Slots int `json:"slots"`
	// Schedule is its budget schedule, read at UTCOffsetS seconds east of
	// UTC, so a peer never wakes it into a window that admits no work
	// (review second-F6). All of it, in order: the first window that
	// covers a time wins (review r2 second-S2).
	Schedule   []config.Window `json:"schedule,omitempty"`
	UTCOffsetS int             `json:"utc_offset_s,omitempty"`
}

// WakeNeed is a queued job that no awake device can take, which a broker
// reports so the mesh can wake a sleeping device that fits it.
type WakeNeed struct {
	Key   string
	Job   string    // "owner/repo: job name", for logs and status
	Since time.Time // when it started waiting
	// Fits reports whether a worker with these base labels and
	// platforms could run the job.
	Fits func(labels []string, platforms []core.Platform) bool
}

// WakeRecord is the last wake of one device, as this device decided it.
type WakeRecord struct {
	Device   string    `json:"device"`
	LastWake time.Time `json:"last_wake"`
	Result   string    `json:"result"` // sent, failed, online, timeout
	Sender   string    `json:"sender,omitempty"`
	Job      string    `json:"job,omitempty"`
	Error    string    `json:"error,omitempty"`
	// OnlineAfterS is how long the device took to answer after the wake.
	OnlineAfterS int64 `json:"online_after_s,omitempty"`
	// Misses counts failed and timed out wakes in a row.
	Misses      int       `json:"misses"`
	NextAllowed time.Time `json:"next_allowed"`
	// repeats counts wakes in a row for the same queued job (key) that
	// came online and didn't take it; they hold the backoff when Misses
	// clear.
	repeats int
	key     string
}

// WakeLog keeps wake records and counters. The daemon makes one and hands
// it to each Node it builds, so a config reload never resets them.
type WakeLog struct {
	mu     sync.Mutex
	recs   map[string]*WakeRecord
	counts map[string]int64
}

// NewWakeLog returns an empty log.
func NewWakeLog() *WakeLog {
	return &WakeLog{recs: map[string]*WakeRecord{}, counts: map[string]int64{}}
}

// Counts is every result's count since the log was made.
func (l *WakeLog) Counts() map[string]int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int64{}
	for _, r := range WakeResults {
		out[r] = l.counts[r]
	}
	return out
}

func (l *WakeLog) record(device string) (WakeRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.recs[device]
	if !ok {
		return WakeRecord{}, false
	}
	return *r, true
}

// allowed reports whether device may be woken at now.
func (l *WakeLog) allowed(device string, now time.Time) bool {
	r, ok := l.record(device)
	return !ok || !now.Before(r.NextAllowed)
}

// pending lists devices whose wake was sent and is still within wakeWait.
func (l *WakeLog) pending(now time.Time) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for d, r := range l.recs {
		if r.Result == WakeSent && now.Sub(r.LastWake) < wakeWait {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// attempt records a wake of device at now: result is WakeSent or
// WakeFailed.
func (l *WakeLog) attempt(device, key, job, sender, result, errText string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.recs[device]
	if r == nil {
		r = &WakeRecord{Device: device}
		l.recs[device] = r
	}
	// Woken for this job already, it came online and didn't take it (a
	// guard, a schedule it changed): that is a miss too, so the backoff
	// grows and it isn't woken every wakeEvery all night (review
	// second-F6). Online for another job, it proved it wakes: start over.
	// The key, not the job's name: a new run of a job with the same name is
	// new work (review r2 first-F1).
	if r.Result == WakeOnline {
		if key != "" && r.key == key {
			r.repeats++
		} else {
			r.repeats = 0
		}
	}
	r.LastWake, r.key, r.Job, r.Sender, r.Result, r.Error, r.OnlineAfterS = now, key, job, sender, result, errText, 0
	if result == WakeFailed {
		r.Misses++
	}
	r.NextAllowed = now.Add(backoff(r.Misses + r.repeats))
	l.counts[result]++
}

// settle turns sent wakes into online or timeout: seen maps a device to
// when it last answered.
func (l *WakeLog) settle(now time.Time, seen func(device string) time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for d, r := range l.recs {
		if r.Result != WakeSent {
			continue
		}
		if at := seen(d); at.After(r.LastWake) {
			r.Result, r.Misses = WakeOnline, 0
			r.OnlineAfterS = int64(at.Sub(r.LastWake).Seconds())
			r.NextAllowed = r.LastWake.Add(backoff(r.repeats))
			l.counts[WakeOnline]++
			continue
		}
		if now.Sub(r.LastWake) >= wakeWait {
			r.Result = WakeTimeout
			r.Misses++
			r.NextAllowed = r.LastWake.Add(backoff(r.Misses + r.repeats))
			l.counts[WakeTimeout]++
		}
	}
}

// backoff is the wait before the next wake after misses failures in a
// row: wakeEvery, doubled per miss, at most wakeBackoffMax.
func backoff(misses int) time.Duration {
	d := wakeEvery
	for i := 0; i < misses && d < wakeBackoffMax; i++ {
		d *= 2
	}
	return min(d, wakeBackoffMax)
}

// knownDevice is a pool device this node has heard from, kept after it
// goes to sleep so it can be woken.
type knownDevice struct {
	ID       string    `json:"id"` // tailnet node ID
	Device   string    `json:"device"`
	Wake     *WakeInfo `json:"wake,omitempty"`
	LastSeen time.Time `json:"last_seen"`
	// Kind is what the node was to this device (owner, guest or host) and
	// Login its tailnet user, for tentmaker devices. An older file has
	// neither; such a device counts as the owner's.
	Kind  string `json:"kind,omitempty"`
	Login string `json:"login,omitempty"`
}

type knownFile struct {
	Version int            `json:"version"`
	Devices []*knownDevice `json:"devices"`
}

// knownTable is the known devices and when they were last saved.
type knownTable struct {
	mu      sync.Mutex
	devs    map[string]*knownDevice // by node ID
	savedAt map[string]time.Time    // LastSeen as last saved, by node ID
	dirty   bool
}

func (n *Node) knownPath() string {
	if n.o.StateDir == "" {
		return ""
	}
	return filepath.Join(n.o.StateDir, "mesh-devices.json")
}

// loadKnown reads the known devices file. A missing or unreadable file is
// an empty table: discovery fills it again.
func (n *Node) loadKnown() {
	n.known.devs, n.known.savedAt = map[string]*knownDevice{}, map[string]time.Time{}
	p := n.knownPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			n.log.Warn("mesh: can't read the known devices; starting empty", "err", err)
		}
		return
	}
	var f knownFile
	if err := json.Unmarshal(b, &f); err != nil {
		n.log.Warn("mesh: the known devices file is damaged; starting empty", "err", err)
		return
	}
	for _, d := range f.Devices {
		if d != nil && d.ID != "" && d.Device != "" {
			n.known.devs[d.ID] = d
			n.known.savedAt[d.ID] = d.LastSeen
		}
	}
}

// noteKnown records a hello from pool peer id at now.
func (n *Node) noteKnown(id string, h helloResponse, kind, login string, now time.Time) {
	n.known.mu.Lock()
	defer n.known.mu.Unlock()
	d := n.known.devs[id]
	if d == nil {
		d = &knownDevice{ID: id}
		n.known.devs[id] = d
		n.known.dirty = true
	}
	if d.Device != h.Device || !sameWake(d.Wake, h.Wake) || d.Kind != kind || d.Login != login {
		n.known.dirty = true
	}
	d.Device, d.Wake, d.LastSeen, d.Kind, d.Login = h.Device, h.Wake, now, kind, login
	if now.Sub(n.known.savedAt[id]) >= knownSaveEvery {
		n.known.dirty = true
	}
}

func sameWake(a, b *WakeInfo) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// saveKnown prunes devices not seen in knownKeep and writes the table when
// it changed.
func (n *Node) saveKnown(now time.Time) {
	n.known.mu.Lock()
	for id, d := range n.known.devs {
		if now.Sub(d.LastSeen) > knownKeep {
			delete(n.known.devs, id)
			delete(n.known.savedAt, id)
			n.known.dirty = true
		}
	}
	if !n.known.dirty {
		n.known.mu.Unlock()
		return
	}
	f := knownFile{Version: 1}
	for _, d := range n.known.devs {
		c := *d
		f.Devices = append(f.Devices, &c)
	}
	sort.Slice(f.Devices, func(i, j int) bool { return f.Devices[i].ID < f.Devices[j].ID })
	n.known.dirty = false
	for _, d := range f.Devices {
		n.known.savedAt[d.ID] = d.LastSeen
	}
	n.known.mu.Unlock()
	p := n.knownPath()
	if p == "" {
		return
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = config.WriteAtomic(p, append(b, '\n'), 0o600)
	}
	if err != nil {
		n.log.Warn("mesh: can't save the known devices", "err", err)
		n.known.mu.Lock()
		n.known.dirty = true
		n.known.mu.Unlock()
	}
}

// knownList is a copy of the known devices, by device name then node ID.
func (n *Node) knownList() []knownDevice {
	n.known.mu.Lock()
	defer n.known.mu.Unlock()
	out := make([]knownDevice, 0, len(n.known.devs))
	for _, d := range n.known.devs {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// knownByName is the most recently seen known device with that name,
// preferring one with wake facts: a guest that announces the name of the
// owner's sleeping device must not stop it being woken.
func (n *Node) knownByName(name string) (knownDevice, bool) {
	var best knownDevice
	found := false
	for _, d := range n.knownList() {
		if d.Device != name || !ownerRecord(d) {
			continue
		}
		if !found || betterRecord(d, best) {
			best, found = d, true
		}
	}
	return best, found
}

// betterRecord: of two records of one name, d stands for it over o when
// only d has wake facts, or both or neither do and d was seen later.
func betterRecord(d, o knownDevice) bool {
	if (d.Wake != nil) != (o.Wake != nil) {
		return d.Wake != nil
	}
	return d.LastSeen.After(o.LastSeen)
}

// ownerRecord: a known device of the pool owner's (or one recorded before
// kinds were, which discovery clears of a non-owner's wake facts). A lent
// device's or a host's record never stands for a name in a wake.
func ownerRecord(d knownDevice) bool { return d.Kind == "" || d.Kind == kindOwner }

// scrubForeignWake drops wake facts recorded for a tailnet node that is not
// this device's own user (recorded before only the same user's were kept).
func (n *Node) scrubForeignWake(self Peer, all []Peer) {
	n.known.mu.Lock()
	defer n.known.mu.Unlock()
	for _, p := range all {
		d := n.known.devs[p.ID]
		if d != nil && d.Wake != nil && !sameUser(self, p.UserID, p.LoginName, p.Tags) {
			d.Wake = nil
			n.known.dirty = true
		}
	}
}

// lastSeen is when the pool owner's device with that name last answered.
func (n *Node) lastSeen(name string) time.Time {
	d, _ := n.knownByName(name)
	return d.LastSeen
}

// awake reports whether d answered its hello in the last awakeWithin.
func awake(d knownDevice, now time.Time) bool { return now.Sub(d.LastSeen) < awakeWithin }

// whyNotWake says why d may not be woken at now, or "" when it may. It
// does not look at the rate limit or at whether d is awake.
func whyNotWake(d knownDevice, now time.Time) string {
	w := d.Wake
	switch {
	case w == nil:
		return "it never reported its LAN (an older Tentmaker)"
	case w.OptOut:
		return "wake = false in its config"
	case w.DoNotDisturb:
		return "its owner paused it (do not disturb)"
	case w.Power == nil || !w.Power.Known:
		return "its power state was unknown when last seen"
	case w.Power.HasBattery && !w.Power.OnAC:
		// No battery is mains power, as the budget reads it: many desktops
		// have no AC supply entry at all (review second-F7).
		return "it was on battery when last seen"
	case w.LAN.MAC == "" || w.LAN.Subnet == "":
		return "it reported no LAN interface"
	case !w.LAN.Wakeable:
		return "its adapter can't wake it: " + w.LAN.Note
	case w.Slots < 1:
		return "its budget fits no job"
	case budget.OffAt(w.Schedule, w.UTCOffsetS, now):
		return "its schedule is off now"
	case len(w.Platforms) == 0:
		return "it reported no platforms it can run"
	case now.Sub(d.LastSeen) > wakeRecent:
		return "not seen in 7 days"
	}
	return ""
}

// refreshWake reads this device's wake facts (join and every heartbeat).
func (n *Node) refreshWake(ctx context.Context) {
	if n.o.Wake == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, wakeInfoTimeout)
	defer cancel()
	w := n.o.Wake(ctx)
	n.wakeMu.Lock()
	n.selfWake = &w
	n.wakeMu.Unlock()
}

func (n *Node) selfWakeInfo() *WakeInfo {
	n.wakeMu.Lock()
	defer n.wakeMu.Unlock()
	if n.selfWake == nil {
		return nil
	}
	w := *n.selfWake
	return &w
}

// errWakeRefused is a wake this device won't send; its text says why.
type errWakeRefused struct{ why string }

func (e errWakeRefused) Error() string { return e.why }

// sendWake sends a magic packet to the known pool device named device,
// from this device. The MAC and broadcast address come from this node's
// own records (the device's hello and this device's LAN), never from the
// request. It returns the time of an earlier send within wakeEvery
// instead of sending again.
func (n *Node) sendWake(ctx context.Context, device, why string) (time.Time, error) {
	if n.o.Sender == nil || n.o.Wake == nil {
		return time.Time{}, errWakeRefused{"this device sends no wake packets"}
	}
	now := n.now()
	d, ok := n.knownByName(device)
	switch {
	case !ok:
		return time.Time{}, errWakeRefused{"not a known pool device"}
	case device == n.o.Device:
		return time.Time{}, errWakeRefused{"that is this device"}
	case awake(d, now):
		return time.Time{}, errWakeRefused{"it is awake"}
	}
	if w := whyNotWake(d, now); w != "" {
		return time.Time{}, errWakeRefused{w}
	}
	self := n.selfWakeInfo()
	if self == nil || self.LAN.Broadcast == "" || self.LAN.Subnet == "" {
		return time.Time{}, errWakeRefused{"this device has no LAN interface"}
	}
	if !SameSubnet(self.LAN.Subnet, d.Wake.LAN.Subnet) {
		return time.Time{}, errWakeRefused{"it is not on this device's LAN"}
	}
	mac, err := net.ParseMAC(d.Wake.LAN.MAC)
	if err != nil || len(mac) != 6 {
		return time.Time{}, errWakeRefused{"its recorded MAC is not a 6-byte MAC"}
	}
	bcast, err := netip.ParseAddr(self.LAN.Broadcast)
	if err != nil || !bcast.Is4() {
		return time.Time{}, errWakeRefused{"this device's broadcast address is not IPv4"}
	}
	n.wakeMu.Lock()
	if at, ok := n.sentAt[device]; ok && now.Sub(at) < wakeEvery {
		n.wakeMu.Unlock()
		return at, nil
	}
	n.sentAt[device] = now
	n.wakeMu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	err = n.o.Sender.Send(sctx, mac, bcast)
	cancel()
	if err != nil {
		n.wakeMu.Lock()
		delete(n.sentAt, device)
		n.wakeMu.Unlock()
		n.log.Warn("mesh: can't send a Wake-on-LAN packet", "device", device, "err", err)
		return time.Time{}, err
	}
	n.log.Info("mesh: sent a Wake-on-LAN packet", "device", device, "mac", d.Wake.LAN.MAC, "broadcast", bcast.String(), "why", why)
	return time.Time{}, nil
}

type wakeRequest struct {
	Device string `json:"device"`
}

type wakeResponse struct {
	Sent bool `json:"sent"`
	// Already is when this device sent one before, within wakeEvery.
	Already time.Time `json:"already,omitzero"`
}

// handleWake is POST /v1/wake: a pool device asks this one to wake a
// sleeping pool device on this device's LAN. Only a device this node knows
// can be named; the request carries no MAC.
func (n *Node) handleWake(w http.ResponseWriter, r *http.Request) {
	var req wakeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Device == "" || len(req.Device) > 128 {
		writeErr(w, http.StatusBadRequest, "device must be 1 to 128 bytes")
		return
	}
	id, _ := r.Context().Value(callerKey{}).(Identity)
	ctx, cancel := context.WithTimeout(r.Context(), n.helloTimeout+whoisTimeout)
	caller, why := n.callerDevice(ctx, id, r.RemoteAddr)
	cancel()
	if why != "" {
		n.refuse(r, caller, why)
		writeErr(w, http.StatusForbidden, "only a pool device may ask for a wake")
		return
	}
	already, err := n.sendWake(r.Context(), req.Device, "asked by "+caller)
	var ref errWakeRefused
	switch {
	case errors.As(err, &ref):
		writeErr(w, http.StatusConflict, ref.why)
	case err != nil:
		writeErr(w, http.StatusBadGateway, "the packet could not be sent")
	default:
		writeJSON(w, http.StatusOK, wakeResponse{Sent: already.IsZero(), Already: already})
	}
}

// wakeLoop runs WakeTick every wakeTickEvery until ctx ends.
func (n *Node) wakeLoop(ctx context.Context) {
	t := time.NewTicker(n.wakeTickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n.WakeTick(ctx)
	}
}

// viewFresh reports whether discovery has run recently, and long enough
// ago for the first time, that a device missing from it is asleep rather
// than not asked yet. A stale view wakes nobody: fail closed.
func (n *Node) viewFresh(now time.Time) bool {
	n.discMu.Lock()
	defer n.discMu.Unlock()
	return !n.discFirst.IsZero() && now.Sub(n.discFirst) >= awakeWithin &&
		now.Sub(n.discAt) < 2*n.discoverEvery
}

// WakeTick settles earlier wakes, then for each job the broker says no
// awake device can take, oldest first, wakes one sleeping device that fits
// it, unless a device already on its way fits it and has a slot to spare
// (each covers as many jobs as its budget's slots).
func (n *Node) WakeTick(ctx context.Context) {
	if n.o.WakeNeeds == nil {
		return
	}
	now := n.now()
	n.wakes.settle(now, n.lastSeen)
	needs := n.o.WakeNeeds()
	if len(needs) == 0 || !n.viewFresh(now) {
		return
	}
	sort.SliceStable(needs, func(i, j int) bool { return needs[i].Since.Before(needs[j].Since) })
	known := n.knownList()
	byName := map[string]knownDevice{}
	for _, d := range known {
		if o, ok := byName[d.Device]; ownerRecord(d) && (!ok || betterRecord(d, o)) {
			byName[d.Device] = d
		}
	}
	// room is how many more waiting jobs each device on its way covers:
	// its budget's slots.
	room := map[string]int{}
	var coming []string
	for _, name := range n.wakes.pending(now) {
		if d, ok := byName[name]; ok && d.Wake != nil {
			room[name] = d.Wake.Slots
			coming = append(coming, name)
		}
	}
	fits := func(need WakeNeed, d knownDevice) bool {
		return d.Wake != nil && need.Fits != nil && need.Fits(d.Wake.Labels, d.Wake.Platforms)
	}
	for _, need := range needs {
		covered := false
		for _, name := range coming {
			if room[name] > 0 && fits(need, byName[name]) {
				room[name]--
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		var best knownDevice
		found := false
		for _, d := range byName {
			if _, on := room[d.Device]; on || d.Device == n.o.Device || awake(d, now) || whyNotWake(d, now) != "" ||
				!n.wakes.allowed(d.Device, now) || !fits(need, d) {
				continue
			}
			if !found || d.LastSeen.After(best.LastSeen) || (d.LastSeen.Equal(best.LastSeen) && d.Device < best.Device) {
				best, found = d, true
			}
		}
		if !found {
			continue
		}
		n.log.Info("mesh: a queued job fits only a sleeping device; waking it", "device", best.Device, "job", need.Job,
			"waiting", now.Sub(need.Since).Round(time.Second).String())
		if n.wakeDevice(ctx, best, need, now) {
			room[best.Device] = best.Wake.Slots - 1
			coming = append(coming, best.Device)
		}
	}
}

// wakeDevice sends the wake from this device when it shares d's LAN, else
// asks up to maxWakeSenders awake pool devices on that LAN, fastest
// first. It records the result and reports whether a packet went out.
func (n *Node) wakeDevice(ctx context.Context, d knownDevice, need WakeNeed, now time.Time) bool {
	var errs []string
	if self := n.selfWakeInfo(); self != nil && n.o.Sender != nil && SameSubnet(self.LAN.Subnet, d.Wake.LAN.Subnet) {
		_, err := n.sendWake(ctx, d.Device, "for "+need.Job)
		if err == nil {
			n.wakes.attempt(d.Device, need.Key, need.Job, n.o.Device, WakeSent, "", now)
			return true
		}
		errs = append(errs, n.o.Device+": "+err.Error())
	}
	known := map[string]knownDevice{}
	for _, k := range n.knownList() {
		known[k.ID] = k
	}
	var senders []peerRef
	for _, p := range n.snapshot() {
		k, ok := known[p.id]
		if p.skipped || !ok || !awake(k, now) || p.hello.Wake == nil || !SameSubnet(p.hello.Wake.LAN.Subnet, d.Wake.LAN.Subnet) {
			continue
		}
		senders = append(senders, p)
	}
	sortByLatency(senders)
	if len(senders) > maxWakeSenders {
		senders = senders[:maxWakeSenders]
	}
	for _, p := range senders {
		cctx, cancel := context.WithTimeout(ctx, n.callTimeout+sendTimeout)
		var resp wakeResponse
		start := time.Now()
		err := n.call(cctx, http.MethodPost, p.base+"/v1/wake", wakeRequest{Device: d.Device}, &resp)
		cancel()
		n.record(p.id, err, time.Since(start))
		if err == nil {
			n.wakes.attempt(d.Device, need.Key, need.Job, p.hello.Device, WakeSent, "", now)
			return true
		}
		errs = append(errs, p.hello.Device+": "+err.Error())
	}
	msg := "no awake pool device shares its LAN"
	if len(errs) > 0 {
		msg = errs[0]
		for _, e := range errs[1:] {
			msg += "; " + e
		}
	}
	n.log.Warn("mesh: can't wake a sleeping device", "device", d.Device, "job", need.Job, "err", msg)
	n.wakes.attempt(d.Device, need.Key, need.Job, "", WakeFailed, msg, now)
	return false
}

// WakeStatus is this device's LAN, the pool devices it knows and its last
// wake of each, for status --json.
type WakeStatus struct {
	Self      *LAN             `json:"self,omitempty"`
	SelfError string           `json:"self_error,omitempty"`
	OptOut    bool             `json:"opt_out,omitempty"`
	Devices   []WakeDevice     `json:"devices"`
	Counts    map[string]int64 `json:"counts"`
}

// WakeDevice is one known pool device and whether it can be woken.
type WakeDevice struct {
	Device   string    `json:"device"`
	Awake    bool      `json:"awake"`
	LastSeen time.Time `json:"last_seen"`
	MAC      string    `json:"mac,omitempty"`
	Subnet   string    `json:"subnet,omitempty"`
	CanWake  bool      `json:"can_wake"`
	// Why says why it can't be woken now ("" when it can, or when it is
	// awake).
	Why  string      `json:"why,omitempty"`
	Last *WakeRecord `json:"last,omitempty"`
}

// WakeStatus reports this device's LAN, every known pool device and the
// last wake this device decided for each.
func (n *Node) WakeStatus() WakeStatus {
	now := n.now()
	st := WakeStatus{Devices: []WakeDevice{}, Counts: n.wakes.Counts()}
	if w := n.selfWakeInfo(); w != nil {
		if w.LAN.MAC != "" {
			l := w.LAN
			st.Self = &l
		}
		st.SelfError, st.OptOut = w.LANError, w.OptOut
	}
	seen := map[string]bool{}
	list := n.knownList()
	sort.SliceStable(list, func(i, j int) bool { return list[i].LastSeen.After(list[j].LastSeen) })
	for _, d := range list {
		if seen[d.Device] {
			continue
		}
		seen[d.Device] = true
		wd := WakeDevice{Device: d.Device, Awake: awake(d, now), LastSeen: d.LastSeen}
		if d.Wake != nil {
			wd.MAC, wd.Subnet = d.Wake.LAN.MAC, d.Wake.LAN.Subnet
		}
		if !wd.Awake {
			wd.Why = whyNotWake(d, now)
			if r, ok := n.wakes.record(d.Device); ok && wd.Why == "" && now.Before(r.NextAllowed) {
				wd.Why = "woken at " + r.LastWake.UTC().Format(time.RFC3339) + "; next try after " + r.NextAllowed.UTC().Format(time.RFC3339)
			}
			wd.CanWake = wd.Why == ""
		}
		if r, ok := n.wakes.record(d.Device); ok {
			wd.Last = &r
		}
		st.Devices = append(st.Devices, wd)
	}
	sort.Slice(st.Devices, func(i, j int) bool { return st.Devices[i].Device < st.Devices[j].Device })
	return st
}
