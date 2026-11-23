package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/mesh"
)

const usageInvite = `tentmaker invite [--trust trusted|public] [--repos owner/a,owner/b] [--ttl 24h] [--json]

Makes a one-time code that lends another tailnet user's device to this
pool. Give the code to them; on their device they run: sudo tentmaker join
The code is shown once and works once, until --ttl (1m to 30d) runs out.

  --trust trusted   runs jobs of --repos (every repo when none are listed),
                    never a fork, pull_request_target or untrusted job
  --trust public    runs only fork pull_request jobs of public repos that
                    set lend_public, with no secrets and no cache writes`

const usageJoin = `tentmaker join [--json]

Lends this device to the pool whose invite code you were given. It asks for
the code (or reads it from stdin); never put the code on the command line.
You keep your budget, pause and drain; leave any time: sudo tentmaker leave`

const usageDevicesAll = `tentmaker devices [--json]
tentmaker devices remove NAME [--json]
tentmaker devices add HOST [--channel stable|dev] [--memory S] [--boost-memory S] [--repo R]... [--json]

With no subcommand, every device in the pool: owner login, trust, repos,
online or asleep, and whether it is lent. remove revokes a lent device at
once: it gets no new work. add installs Tentmaker on your own device.`

type inviteView struct {
	OK      bool      `json:"ok"`
	Code    string    `json:"code"`
	Trust   string    `json:"trust"`
	Repos   []string  `json:"repos"`
	Expires time.Time `json:"expires"`
}

func (a *app) cmdInvite(args []string) (any, error) {
	f := a.flags("invite", usageInvite)
	trust := f.String("trust", core.TrustTrusted, "trusted or public")
	repos := f.String("repos", "", "owner/a,owner/b")
	ttl := f.String("ttl", "24h", "how long the code works, like 1h or 7d")
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+usageInvite, "unexpected argument %q", pos[0])
	}
	if !core.ValidLendTrust(*trust) {
		return nil, usage("use --trust trusted or public", "--trust %q is not trusted or public", *trust)
	}
	var list []string
	for _, r := range strings.Split(*repos, ",") {
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		if err := validRepo(r); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	if *trust == core.TrustPublic && len(list) > 0 {
		return nil, usage("drop --repos; set lend_public on each public repo instead", "--repos is for --trust trusted")
	}
	d, err := parseTTL(*ttl)
	if err != nil || d < mesh.MinInviteTTL || d > mesh.MaxInviteTTL {
		return nil, usage("write it like 1h or 7d, from 1m to 30d", "--ttl %q is not a duration from 1m to 30d", *ttl)
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return nil, err
	}
	var ic mesh.InviteCode
	if err := a.controlClient(cfg).Call(a.ctx, "/v1/invite", control.InviteRequest{Trust: *trust, Repos: list, TTLS: int64(d / time.Second)}, &ic); err != nil {
		return nil, serviceErr(err)
	}
	if !a.json {
		fmt.Fprintln(a.env.Stdout, ic.Code)
		fmt.Fprintf(a.env.Stderr, "Give this code to the device's owner; it works once, until %s.\nOn their device: sudo tentmaker join\n",
			ic.Expires.Local().Format("2006-01-02 15:04"))
	}
	return inviteView{OK: true, Code: ic.Code, Trust: ic.Trust, Repos: nonNil(ic.Repos), Expires: ic.Expires}, nil
}

// parseTTL reads a Go duration or a whole number of days ("7d").
func parseTTL(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("bad days")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

type lentView struct {
	OK bool `json:"ok"`
	mesh.Lent
}

func (a *app) cmdJoin(args []string) (any, error) {
	f := a.flags("join", usageJoin)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		// Never echo it: it may be the code.
		return nil, usage("run: sudo tentmaker join, then paste the code when asked", "join takes no argument; the code is read from the terminal or stdin")
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return nil, err
	}
	code, err := a.readCode()
	if err != nil {
		return nil, err
	}
	var l mesh.Lent
	if err := a.controlClient(cfg).CallFor(a.ctx, control.JoinTimeout+5*time.Second, "/v1/join", control.JoinRequest{Code: code}, &l); err != nil {
		return nil, serviceErr(err)
	}
	repos := "every repo it allows"
	if len(l.Repos) > 0 {
		repos = strings.Join(l.Repos, ", ")
	}
	a.out("lent to %s's pool %q (trust %s: %s)\nleave any time: sudo tentmaker leave\n", l.OwnerLogin, l.Pool, l.Trust, repos)
	return lentView{OK: true, Lent: l}, nil
}

// readCode reads an invite code from the terminal without echo, or from
// stdin when it is not a terminal.
func (a *app) readCode() (string, error) {
	var b []byte
	var err error
	if a.env.StdinTTY {
		fmt.Fprint(a.env.Stderr, "Invite code (it won't show): ")
		b, err = a.env.ReadSecret()
		fmt.Fprintln(a.env.Stderr)
	} else {
		b, err = io.ReadAll(io.LimitReader(a.in, 4096))
	}
	if err != nil {
		return "", fail("paste the code when asked, or pipe it on stdin", "read the code: %v", err)
	}
	code := strings.TrimSpace(string(b))
	if code == "" || strings.ContainsAny(code, " \t\r\n") {
		return "", usage("paste only the code tentmaker invite printed", "no invite code was given")
	}
	return code, nil
}

func (a *app) cmdLeave(args []string) (any, error) {
	f := a.flags("leave", commands["leave"].usage)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) > 0 {
		return nil, usage("usage: "+commands["leave"].usage, "unexpected argument %q", pos[0])
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return nil, err
	}
	var l mesh.Lent
	if err := a.controlClient(cfg).Call(a.ctx, "/v1/leave", nil, &l); err != nil {
		return nil, serviceErr(err)
	}
	a.out("left %s's pool %q; this device is back in its own pool\n", l.OwnerLogin, l.Pool)
	return lentView{OK: true, Lent: l}, nil
}

type devicesView struct {
	Devices []mesh.PoolDevice `json:"devices"`
}

func (a *app) listDevices() (any, error) {
	cfg, err := a.mustConfig()
	if err != nil {
		return nil, err
	}
	var devs []mesh.PoolDevice
	if err := a.controlClient(cfg).Get(a.ctx, "/v1/devices", &devs); err != nil {
		return nil, serviceErr(err)
	}
	if !a.json {
		tw := tabwriter.NewWriter(a.env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "DEVICE\tOWNER\tTRUST\tREPOS\tSTATE\tLENT")
		for _, d := range devs {
			name := d.Device
			if d.Self {
				name += " (this)"
			}
			repos := "all"
			if len(d.Repos) > 0 {
				repos = strings.Join(d.Repos, ",")
			}
			lent := "no"
			if d.Lent {
				lent = "yes"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", name, firstNonEmpty(d.Owner, "?"), d.Trust, repos, d.State, lent)
		}
		tw.Flush()
	}
	return devicesView{Devices: nonNil(devs)}, nil
}

type deviceRemoveView struct {
	OK      bool         `json:"ok"`
	Removed []mesh.Guest `json:"removed"`
}

func (a *app) removeDevice(args []string) (any, error) {
	f := a.flags("devices remove", usageDevicesAll)
	pos, err := a.parse(f, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 {
		return nil, usage("usage: tentmaker devices remove NAME", "devices remove needs one NAME")
	}
	if err := a.needRoot(); err != nil {
		return nil, err
	}
	cfg, err := a.mustConfig()
	if err != nil {
		return nil, err
	}
	var gs []mesh.Guest
	if err := a.controlClient(cfg).Call(a.ctx, "/v1/devices/remove", control.RemoveDeviceRequest{Name: pos[0]}, &gs); err != nil {
		return nil, serviceErr(err)
	}
	a.out("removed %s from the pool; it gets no new work, and jobs it runs now end by its drain rules\n", pos[0])
	return deviceRemoveView{OK: true, Removed: nonNil(gs)}, nil
}
