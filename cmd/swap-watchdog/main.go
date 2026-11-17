// swap-watchdog polls system memory pressure and, on a rising edge into
// "critical", reveals an already-running Force Quit Monitor window via
// kdotool rather than spawning anything new at the exact moment the system
// is least able to launch a process.
//
// `swap-watchdog --reveal` shows that window once and exits: the start
// menu's Force Quit Monitor entry runs it, so opening it from the menu brings
// up the instance that's already running instead of starting a second one.
package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	pollInterval      = 3 * time.Second
	defaultSwapPct    = 1.0  // percent SwapFree/SwapTotal below which swap counts as exhausted
	defaultPSI        = 10.0 // percent "full avg10" above which memory pressure counts as critical
	forceQuitWinTitle = "Force Quit Monitor"
)

func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// swapStatus reports whether swap is critically exhausted. ok is false if
// there's no swap configured at all (nothing to measure).
func swapStatus(pctThreshold float64) (critical bool, pct float64, ok bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return false, 0, false
	}
	defer f.Close()

	var total, free float64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "SwapTotal:":
			total, _ = strconv.ParseFloat(fields[1], 64)
		case "SwapFree:":
			free, _ = strconv.ParseFloat(fields[1], 64)
		}
	}
	if total <= 0 {
		return false, 0, false
	}
	pct = (free / total) * 100
	return pct < pctThreshold, pct, true
}

// memoryPressure reads the kernel PSI "full avg10" figure — the same signal
// systemd-oomd uses — for /proc/pressure/memory.
func memoryPressure(psiThreshold float64) (critical bool, avg10 float64, ok bool) {
	f, err := os.Open("/proc/pressure/memory")
	if err != nil {
		return false, 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if val, found := strings.CutPrefix(field, "avg10="); found {
				avg10, _ = strconv.ParseFloat(val, 64)
				return avg10 > psiThreshold, avg10, true
			}
		}
	}
	return false, 0, false
}

// findForceQuitWindow returns the Force Quit Monitor's KWin window ID.
func findForceQuitWindow() (string, error) {
	out, err := exec.Command("kdotool", "search", "-t", forceQuitWinTitle).Output()
	if err != nil {
		return "", fmt.Errorf("kdotool search failed: %w", err)
	}
	id := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if id == "" {
		return "", fmt.Errorf("no window titled %q found", forceQuitWinTitle)
	}
	return id, nil
}

// revealForceQuitWindow un-minimizes and activates the window; keepAbove
// also pins it on top (for a crisis, so it isn't buried again).
func revealForceQuitWindow(keepAbove bool) error {
	id, err := findForceQuitWindow()
	if err != nil {
		return err
	}
	args := []string{"windowstate", "--remove", "MINIMIZED", id}
	if keepAbove {
		args = []string{"windowstate", "--remove", "MINIMIZED", "--add", "ABOVE", id}
	}
	if err := exec.Command("kdotool", args...).Run(); err != nil {
		return fmt.Errorf("kdotool windowstate failed: %w", err)
	}
	if err := exec.Command("kdotool", "windowactivate", id).Run(); err != nil {
		return fmt.Errorf("kdotool windowactivate failed: %w", err)
	}
	return nil
}

// revealFromMenu shows the running Force Quit Monitor. If it isn't running
// (its service stopped, or it was killed), it starts the service and waits
// for the window, which minimizes itself once it's up, then shows it.
func revealFromMenu() error {
	if revealForceQuitWindow(false) == nil {
		return nil
	}
	if err := exec.Command("systemctl", "--user", "start", "force-quit-gui.service").Run(); err != nil {
		return fmt.Errorf("couldn't start force-quit-gui.service: %w", err)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if _, err := findForceQuitWindow(); err == nil {
			// Let it minimize itself first (it does right after it's
			// mapped), or it would do so after being shown.
			time.Sleep(time.Second)
			return revealForceQuitWindow(false)
		}
	}
	return fmt.Errorf("the Force Quit Monitor window didn't appear")
}

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] != "--reveal" {
			fmt.Fprintf(os.Stderr, "usage: %s [--reveal]\n", os.Args[0])
			os.Exit(2)
		}
		if err := revealFromMenu(); err != nil {
			fmt.Fprintf(os.Stderr, "swap-watchdog: %v\n", err)
			os.Exit(1)
		}
		return
	}

	stateDir := filepath.Join(os.Getenv("HOME"), ".local", "state", "swap-watchdog")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		log.Fatalf("could not create state dir: %v", err)
	}
	logFile, err := os.OpenFile(filepath.Join(stateDir, "watchdog.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("could not open log file: %v", err)
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)

	swapPctThreshold := envFloat("SWAP_PCT_THRESHOLD", defaultSwapPct)
	psiThreshold := envFloat("PSI_THRESHOLD", defaultPSI)

	logger.Printf("started (swap threshold=%.2f%%, psi threshold=%.2f)", swapPctThreshold, psiThreshold)

	wasCritical := false
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for range ticker.C {
		swapCrit, swapPct, swapOK := swapStatus(swapPctThreshold)
		psiCrit, psiAvg10, psiOK := memoryPressure(psiThreshold)

		critical := (swapOK && swapCrit) || (psiOK && psiCrit)

		if critical && !wasCritical {
			logger.Printf("TRIGGER: swap_free_pct=%.2f psi_full_avg10=%.2f", swapPct, psiAvg10)
			if err := revealForceQuitWindow(true); err != nil {
				logger.Printf("failed to reveal force-quit window: %v", err)
			}
		}
		wasCritical = critical
	}
}
