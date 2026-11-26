package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"time"
)

// Totals is a rollup of any set of records.
type Totals struct {
	Requests     int64   `json:"requests"`
	Input        int64   `json:"input"`
	Output       int64   `json:"output"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"`
	Tokens       int64   `json:"tokens"`
	USD          float64 `json:"usd"`
	PaidUSD      float64 `json:"paid_usd"`
	Failovers    int64   `json:"failovers"`
	Subscription int64   `json:"subscription_requests"`
}

// add folds one record into t.
func (t *Totals) add(r Record) {
	t.Requests++
	t.Input += r.InputTokens
	t.Output += r.OutputTokens
	t.CacheRead += r.CacheReadTokens
	t.CacheWrite += r.CacheWriteToken
	t.Tokens += r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteToken
	t.USD += r.USD
	if r.Billing != BillingSubscription {
		t.PaidUSD += r.USD
	} else {
		t.Subscription++
	}
	if r.Failover {
		t.Failovers++
	}
}

// Day is one local calendar day: totals overall and per provider.
type Day struct {
	Day       string            `json:"day"`
	Totals    Totals            `json:"totals"`
	Providers map[string]Totals `json:"providers"`
	// Hours holds per-provider request counts for the day's 24 hours, so the
	// tab can draw the stacked hour chart without a second pass.
	Hours []map[string]int64 `json:"hours"`
}

// Summary is the read-side view of the ledger.
type Summary struct {
	// Days is oldest-first. Only days with at least one record appear.
	Days []Day `json:"days"`
	// Recent is the last requests, newest first.
	Recent []Record `json:"recent"`
	// Since is the timestamp of the oldest record kept by this read.
	Since *time.Time `json:"since,omitempty"`
	// Truncated is true when the ledger was larger than one read covers, so
	// Days and Recent reflect only its newest part.
	Truncated bool `json:"truncated"`
}

const (
	// maxScanBytes bounds how much of the ledger one read parses. A ledger
	// below its rotation size is read whole; the bound only keeps a file
	// grown some other way (copied in, hand-edited) from making a read slow.
	maxScanBytes = maxLedgerBytes
	// maxRecent bounds the newest-requests list a read keeps.
	maxRecent = 200
)

// Read parses the ledger at path. A missing file is an empty summary; blank
// and unparseable lines are skipped (a torn append must not hide the history
// behind it).
//
// loc is the timezone days are bucketed in (the operator's local zone): a
// "day" is the day they experienced, not a UTC one.
func Read(path string, loc *time.Location) (Summary, error) {
	out := Summary{Days: []Day{}, Recent: []Record{}}
	if path == "" {
		return out, nil
	}
	if loc == nil {
		loc = time.Local
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	defer func() { _ = f.Close() }()

	if info, err := f.Stat(); err == nil && info.Size() > maxScanBytes {
		if _, err := f.Seek(info.Size()-maxScanBytes, io.SeekStart); err == nil {
			out.Truncated = true
			// Drop the first, almost certainly partial, line of the window.
			br := bufio.NewReaderSize(f, 256*1024)
			_, _ = br.ReadString('\n')
			return scan(br, loc, out)
		}
	}
	return scan(bufio.NewReaderSize(f, 256*1024), loc, out)
}

func scan(br *bufio.Reader, loc *time.Location, out Summary) (Summary, error) {
	byDay := map[string]*Day{}
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var rec Record
			if json.Unmarshal(line, &rec) == nil && !rec.At.IsZero() {
				if out.Since == nil || rec.At.Before(*out.Since) {
					at := rec.At
					out.Since = &at
				}
				addRecord(byDay, rec, loc)
				out.Recent = append(out.Recent, rec)
				if len(out.Recent) > maxRecent {
					out.Recent = out.Recent[len(out.Recent)-maxRecent:]
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
	}
	days := make([]Day, 0, len(byDay))
	for _, d := range byDay {
		days = append(days, *d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })
	out.Days = days
	// Newest first for the caller.
	for i, j := 0, len(out.Recent)-1; i < j; i, j = i+1, j-1 {
		out.Recent[i], out.Recent[j] = out.Recent[j], out.Recent[i]
	}
	return out, nil
}

func addRecord(byDay map[string]*Day, rec Record, loc *time.Location) {
	local := rec.At.In(loc)
	key := local.Format("2006-01-02")
	d := byDay[key]
	if d == nil {
		d = &Day{Day: key, Providers: map[string]Totals{}, Hours: make([]map[string]int64, 24)}
		for h := range d.Hours {
			d.Hours[h] = map[string]int64{}
		}
		byDay[key] = d
	}
	d.Totals.add(rec)
	p := d.Providers[rec.Provider]
	p.add(rec)
	d.Providers[rec.Provider] = p
	d.Hours[local.Hour()][rec.Provider]++
}

// DayFor returns the rollup for a local calendar day, or an empty one.
func (s Summary) DayFor(day string) Day {
	for _, d := range s.Days {
		if d.Day == day {
			return d
		}
	}
	d := Day{Day: day, Providers: map[string]Totals{}, Hours: make([]map[string]int64, 24)}
	for h := range d.Hours {
		d.Hours[h] = map[string]int64{}
	}
	return d
}

// Alert tracks the paid-spend notification. The state is a small JSON file
// next to the ledger so a restart does not re-announce a threshold that was
// already announced.
type Alert struct {
	// StepUSD is the growth between announcements.
	StepUSD float64
}

// AlertState is what Alert persists.
type AlertState struct {
	// Day is the local day the alert was last evaluated for.
	Day string `json:"day"`
	// Cents is the highest paid-spend cent count announced that day.
	Cents int64 `json:"cents"`
}

// Decide reports whether an alert is due for paidUSD on day, given the stored
// state, and returns the state to persist. The rule: announce when the day's
// paid spend first goes above $0, then each time it has grown by at least
// stepUSD since the last announcement. State is per day, so the first paid
// cent of a new day announces again; an unchanged or smaller total never does.
func Decide(state AlertState, day string, paidUSD float64, stepUSD float64) (AlertState, bool) {
	if stepUSD <= 0 {
		stepUSD = 0.50
	}
	if state.Day != day {
		state = AlertState{Day: day}
	}
	cents := int64(paidUSD*100 + 0.5)
	if cents <= 0 {
		return state, false
	}
	step := int64(stepUSD*100 + 0.5)
	if state.Cents > 0 && cents-state.Cents < step {
		return state, false
	}
	state.Cents = cents
	return state, true
}

// LoadAlertState reads the alert state file; a missing or unreadable file is
// the zero state.
func LoadAlertState(path string) AlertState {
	var st AlertState
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st)
	return st
}

// SaveAlertState writes the alert state atomically.
func SaveAlertState(path string, st AlertState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
