package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/client"
)

// linkDaemon is the part of the daemon client `pilothouse link` uses.
type linkDaemon interface {
	LinkCode(ctx context.Context) (api.LinkCode, error)
	LinkJoin(ctx context.Context, j api.LinkJoin) (api.LinkJoined, error)
	LinkStatus(ctx context.Context) (api.LinkStatus, error)
	LinkTrust(ctx context.Context, t api.LinkTrust) (int, error)
	LinkRemove(ctx context.Context, id string) (api.LinkPeer, error)
}

// linkClient is a seam for tests.
var linkClient = func() linkDaemon { return client.New() }

func newLinkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Show sessions from several machines in one place",
		Long: `Links machines so one of them, the hub, shows every machine's sessions
and requests in its app, and your phone answers them all from there.

On the hub (the machine your phone is paired with):
  pilothouse link add

On each other machine, run the command it prints:
  pilothouse link join https://studio.tailnet.ts.net XXXX-XXXX-XXXX

Every machine needs remote access on (` + "`pilothouse remote tailscale`" + `). When you
join, you confirm which of the hub's devices may answer this machine's
requests: each linked machine checks the device's signature itself, so the
hub can't answer for you.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runLinkLs(cmd, false) },
	}
	cmd.AddCommand(newLinkAddCmd(), newLinkJoinCmd(), newLinkLsCmd(), newLinkTrustCmd(), newLinkRemoveCmd())
	return cmd
}

func newLinkAddCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Make a one-time code to link another machine to this one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := linkClient().LinkCode(rmtCtx(cmd))
			if err != nil {
				return rmtErr(err, "pilothouse link add")
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(c)
			}
			if c.URL == "" {
				fmt.Fprintln(out, "! Remote access is off, so other machines can't reach this one.")
				fmt.Fprintln(out, "  Run `pilothouse remote tailscale` here first, then `pilothouse link add` again.")
				return ExitError{Code: 1}
			}
			mins := max(int(c.ExpiresAt.Sub(rmtNow()).Round(time.Minute).Minutes()), 1)
			fmt.Fprintln(out, "On the machine to link, run:")
			fmt.Fprintln(out)
			fmt.Fprintf(out, "  pilothouse link join %s %s\n", c.URL, c.Code)
			fmt.Fprintln(out)
			fmt.Fprintf(out, "The code works once and expires in %d min (%s).\n", mins, c.ExpiresAt.Local().Format("15:04"))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newLinkJoinCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "join <hub URL> <code>",
		Short: "Link this machine to a hub (run what `pilothouse link add` printed)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := rmtCtx(cmd)
			lc := linkClient()
			j, err := lc.LinkJoin(ctx, api.LinkJoin{URL: args[0], Code: args[1]})
			if err != nil {
				return rmtErr(err, "pilothouse link join")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "✓ Linked to %s (%s).\n", j.Hub.Machine, j.Hub.URL)
			pending := untrusted(j.Devices)
			if len(pending) == 0 {
				fmt.Fprintf(out, "%s has no paired devices yet. After you pair one there, run `pilothouse link trust` here.\n", j.Hub.Machine)
				return nil
			}
			return linkConfirm(ctx, cmd, lc, j.Hub.Machine, pending, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "trust the hub's devices without asking")
	return cmd
}

func untrusted(ds []api.LinkDevice) []api.LinkDevice {
	var out []api.LinkDevice
	for _, d := range ds {
		if !d.Trusted && d.RevokedAt == nil {
			out = append(out, d)
		}
	}
	return out
}

// linkConfirm shows the devices and asks before trusting them.
func linkConfirm(ctx context.Context, cmd *cobra.Command, lc linkDaemon, hub string, ds []api.LinkDevice, yes bool) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\nThese devices are paired with %s. Trusted devices can answer this machine's\nrequests, reply to its agents and type into their terminals. Check each key\nagainst the one shown on %s (`pilothouse devices`, or Settings in the app):\n\n", hub, hub)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, d := range ds {
		fmt.Fprintf(tw, "  %s\t%s\tkey %s\n", d.Name, d.ID, d.Fingerprint)
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
	ui := &setupUI{w: out, in: bufio.NewReader(cmd.InOrStdin()), yes: yes}
	if !ui.ask("Trust them on this machine?") {
		fmt.Fprintln(out, "Not trusted. They can see this machine's sessions but not act on them. Run `pilothouse link trust` any time.")
		return nil
	}
	want := make([]api.LinkTrustDevice, len(ds))
	for i, d := range ds {
		want[i] = api.LinkTrustDevice{ID: d.ID, Fingerprint: d.Fingerprint}
	}
	n, err := lc.LinkTrust(ctx, api.LinkTrust{Devices: want})
	if err != nil {
		return rmtErr(err, "pilothouse link trust")
	}
	fmt.Fprintf(out, "✓ Trusted %d device%s: %s can now answer this machine's requests from %s.\n", n, plural(n), pluralThey(n), hub)
	return nil
}

func newLinkLsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"status"},
		Short:   "Show linked machines",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runLinkLs(cmd, asJSON) },
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func runLinkLs(cmd *cobra.Command, asJSON bool) error {
	st, err := linkClient().LinkStatus(rmtCtx(cmd))
	if err != nil {
		return rmtErr(err, "pilothouse link")
	}
	out := cmd.OutOrStdout()
	if asJSON {
		return json.NewEncoder(out).Encode(st)
	}
	printLinkStatus(out, st)
	return nil
}

func printLinkStatus(out io.Writer, st api.LinkStatus) {
	switch {
	case st.Hub != nil:
		state := "reachable"
		if !st.Hub.Online {
			state = "not reachable from here"
		}
		fmt.Fprintf(out, "%s is linked to %s (%s), %s.\n", st.Machine, st.Hub.Machine, st.Hub.URL, state)
		if len(st.Devices) > 0 {
			fmt.Fprintln(out, "\nDevices of the hub:")
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, d := range st.Devices {
				s := "trusted"
				switch {
				case d.RevokedAt != nil:
					s = "revoked"
				case !d.Trusted:
					s = "not trusted (run `pilothouse link trust`)"
				}
				fmt.Fprintf(tw, "  %s\t%s\tkey %s\t%s\n", d.Name, d.ID, d.Fingerprint, s)
			}
			_ = tw.Flush()
		}
	case len(st.Linked) > 0:
		fmt.Fprintf(out, "Machines linked to %s:\n", st.Machine)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, p := range st.Linked {
			s := "online"
			if !p.Online {
				s = "offline"
				if p.Error != "" {
					s += ": " + p.Error
				}
			}
			v := p.Version
			if v == "" {
				v = "—"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", p.Machine, p.URL, v, s)
		}
		_ = tw.Flush()
	default:
		fmt.Fprintln(out, "No linked machines. To see another machine's sessions here, run `pilothouse link add`.")
	}
}

func newLinkTrustCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "trust",
		Short: "Trust the hub's new devices on this machine",
		Long: `Lists the hub's devices this machine doesn't trust yet (newly paired, or
paired again with a new key) and asks before trusting them. Until then they
can see this machine's sessions but not answer, reply or type.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := rmtCtx(cmd)
			lc := linkClient()
			st, err := lc.LinkStatus(ctx)
			if err != nil {
				return rmtErr(err, "pilothouse link trust")
			}
			if st.Hub == nil {
				return fmt.Errorf("this machine isn't linked to a hub (see `pilothouse link join`)")
			}
			pending := untrusted(st.Devices)
			if len(pending) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Every device of %s is trusted here.\n", st.Hub.Machine)
				return nil
			}
			return linkConfirm(ctx, cmd, lc, st.Hub.Machine, pending, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "trust without asking")
	return cmd
}

func newLinkRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <machine>",
		Short: "Unlink a machine (on the hub), or leave the hub (on a linked machine)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := linkClient().LinkRemove(rmtCtx(cmd), strings.TrimSpace(args[0]))
			if err != nil {
				return rmtErr(err, "pilothouse link remove")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Unlinked %s.\n", p.Machine)
			return nil
		},
	}
}

func pluralThey(n int) string {
	if n == 1 {
		return "it"
	}
	return "they"
}
