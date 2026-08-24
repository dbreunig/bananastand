// bananastand — estimate what your RAM and drives are worth today.
//
// The tool detects storage with lsblk and RAM with dmidecode (falling
// back to /proc/meminfo), prices drives against diskprices.com listings,
// prices RAM against ramstickprices.com listings (matched by DDR
// generation, capacity, ECC, and speed), and reports the change since
// the last recorded run.
//
// It builds as a single static binary with no dependencies outside the
// Go standard library.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	version   = "0.1.0"
	diskURL   = "https://diskprices.com/?locale=us&condition=new"
	ramURL    = "https://ramstickprices.com/"
	userAgent = "bananastand/0.1 (personal CLI; at most one request per source per hour)"
	cacheTTL  = time.Hour
)

// Rough replacement rates, used offline or when no listings match.
var diskFallbackPerGB = map[string]float64{"nvme": 0.07, "ssd": 0.06, "hdd": 0.018}
var ramFallbackPerGB = map[string]float64{
	"ddr2": 4.00, "ddr3": 2.50, "ddr4": 7.00, "ddr5": 11.00, "unknown": 7.00,
}

// ------------------------------------------------------------------ types

type component struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Kind  string  `json:"kind"`
	Bytes int64   `json:"bytes"`
	Gen   string  `json:"gen,omitempty"`
	ECC   bool    `json:"ecc,omitempty"`
	Speed int     `json:"speed,omitempty"`
	Value float64 `json:"value"`
	Basis string  `json:"basis"`
}

type histComponent struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type histEntry struct {
	TS         string          `json:"ts"`
	Total      float64         `json:"total"`
	Components []histComponent `json:"components"`
}

type diskListing struct {
	Kind  string  `json:"kind"`
	GB    float64 `json:"gb"`
	Price float64 `json:"price"`
}

type ramListing struct {
	Gen   string  `json:"gen"`
	GB    float64 `json:"gb"`
	Price float64 `json:"price"`
	ECC   bool    `json:"ecc"`
	Speed int     `json:"speed"`
}

type config struct {
	RAMPricePerGB float64            `json:"ram_price_per_gb"`
	DiskFallback  map[string]float64 `json:"fallback_price_per_gb"`
	RAMFallback   map[string]float64 `json:"ram_fallback_price_per_gb"`
}

// ------------------------------------------------------------------ paths

// realHome returns the invoking user's home, so sudo runs share one history.
func realHome() string {
	if os.Geteuid() == 0 {
		if su := os.Getenv("SUDO_USER"); su != "" && su != "root" {
			if u, err := user.Lookup(su); err == nil {
				return u.HomeDir
			}
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

// fixOwner hands files created under sudo back to the invoking user.
func fixOwner(path string) {
	if os.Geteuid() != 0 {
		return
	}
	su := os.Getenv("SUDO_USER")
	if su == "" || su == "root" {
		return
	}
	u, err := user.Lookup(su)
	if err != nil {
		return
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 == nil && err2 == nil {
		_ = os.Chown(path, uid, gid)
	}
}

func dataDir() string {
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		root = filepath.Join(realHome(), ".local", "share")
	}
	d := filepath.Join(root, "bananastand")
	_ = os.MkdirAll(d, 0o755)
	fixOwner(d)
	return d
}

func loadConfig() config {
	root := os.Getenv("XDG_CONFIG_HOME")
	if root == "" {
		root = filepath.Join(realHome(), ".config")
	}
	var cfg config
	b, err := os.ReadFile(filepath.Join(root, "bananastand", "config.json"))
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read config: %v\n", err)
	}
	return cfg
}

func mergedRates(defaults, override map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

// ------------------------------------------------------------- formatting

func commaFmt(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	dot := strings.Index(s, ".")
	intPart, frac := s[:dot], s[dot:]
	var b strings.Builder
	for i := 0; i < len(intPart); i++ {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(intPart[i])
	}
	out := b.String() + frac
	if neg {
		out = "-" + out
	}
	return out
}

func fmtMoney(v float64) string { return "$" + commaFmt(v) }

func fmtDelta(v float64) string {
	if v >= 0 {
		return "+$" + commaFmt(v)
	}
	return "-$" + commaFmt(-v)
}

func fmtSize(n float64) string {
	if n >= 1e12 {
		return strconv.FormatFloat(n/1e12, 'f', 1, 64) + " TB"
	}
	return strconv.FormatFloat(n/1e9, 'f', 0, 64) + " GB"
}

func padR(s string, w int) string {
	if d := w - utf8.RuneCountInString(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func padL(s string, w int) string {
	if d := w - utf8.RuneCountInString(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// ---------------------------------------------------------------- parsing

var (
	rePrice    = regexp.MustCompile(`([\d,]+(?:\.\d+)?)`)
	reCapacity = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(tb|gb|mb)`)
	reGen      = regexp.MustCompile(`ddr(\d)`)
	rePCGen    = regexp.MustCompile(`pc(\d)-`)
	reNonECC   = regexp.MustCompile(`non[\s-]?ecc`)
	reECC      = regexp.MustCompile(`\becc\b|\b(?:lr|fb|r)dimm\b|registered|fully buffered`)
	reSpeed    = regexp.MustCompile(`(\d{3,5})\s*(?:mhz|mt/s)`)
)

func parsePrice(text string) float64 {
	m := rePrice.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	return v
}

func parseCapacityGB(text string) float64 {
	m := reCapacity.FindStringSubmatch(strings.ToLower(text))
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	switch m[2] {
	case "tb":
		return n * 1000
	case "mb":
		return n / 1000
	}
	return n
}

func classifyTech(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "nvme"):
		return "nvme"
	case strings.Contains(t, "ssd"), strings.Contains(t, "solid state"):
		return "ssd"
	case strings.Contains(t, "hdd"), strings.Contains(t, "rpm"),
		strings.Contains(t, "hard drive"), strings.Contains(t, "hard disk"):
		return "hdd"
	}
	return ""
}

func classifyRAMGeneration(text string) string {
	t := strings.ToLower(text)
	if m := reGen.FindStringSubmatch(t); m != nil {
		return "ddr" + m[1]
	}
	if m := rePCGen.FindStringSubmatch(t); m != nil && strings.Contains("2345", m[1]) {
		return "ddr" + m[1]
	}
	return ""
}

func looksECC(text string) bool {
	t := strings.ToLower(text)
	if reNonECC.MatchString(t) {
		return false
	}
	return reECC.MatchString(t)
}

func parseSpeedMTS(text string) int {
	m := reSpeed.FindStringSubmatch(strings.ToLower(text))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ------------------------------------------------ minimal HTML table reader

type tableCell struct {
	header bool
	text   string
}

type tableRow struct {
	attrs map[string]string
	cells []tableCell
}

type tableData struct {
	headers []string // every th text, in document order
	rows    []tableRow
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// indexTag finds "<name" at a real tag boundary in the lowercased doc.
func indexTag(lower, name string, from int) int {
	needle := "<" + name
	for {
		i := strings.Index(lower[from:], needle)
		if i < 0 {
			return -1
		}
		i += from
		j := i + len(needle)
		if j >= len(lower) {
			return i
		}
		if c := lower[j]; isSpaceByte(c) || c == '>' || c == '/' {
			return i
		}
		from = i + 1
	}
}

// tagEnd finds the '>' closing the tag that starts at start, respecting quotes.
func tagEnd(s string, start int) int {
	var inQ byte
	for i := start; i < len(s); i++ {
		c := s[i]
		if inQ != 0 {
			if c == inQ {
				inQ = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQ = c
			continue
		}
		if c == '>' {
			return i
		}
	}
	return len(s) - 1
}

func parseAttrs(tag string) map[string]string {
	attrs := map[string]string{}
	i := 1
	for i < len(tag) && !isSpaceByte(tag[i]) && tag[i] != '>' {
		i++ // skip the tag name
	}
	for i < len(tag) {
		for i < len(tag) && isSpaceByte(tag[i]) {
			i++
		}
		if i >= len(tag) || tag[i] == '>' || tag[i] == '/' {
			break
		}
		ks := i
		for i < len(tag) && tag[i] != '=' && tag[i] != '>' && !isSpaceByte(tag[i]) {
			i++
		}
		key := strings.ToLower(tag[ks:i])
		val := ""
		for i < len(tag) && isSpaceByte(tag[i]) {
			i++
		}
		if i < len(tag) && tag[i] == '=' {
			i++
			for i < len(tag) && isSpaceByte(tag[i]) {
				i++
			}
			if i < len(tag) && (tag[i] == '"' || tag[i] == '\'') {
				q := tag[i]
				i++
				vs := i
				for i < len(tag) && tag[i] != q {
					i++
				}
				val = tag[vs:i]
				if i < len(tag) {
					i++
				}
			} else {
				vs := i
				for i < len(tag) && !isSpaceByte(tag[i]) && tag[i] != '>' {
					i++
				}
				val = tag[vs:i]
			}
		}
		if key != "" {
			attrs[key] = html.UnescapeString(val)
		}
	}
	return attrs
}

func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	var inQ byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inTag {
			if inQ != 0 {
				if c == inQ {
					inQ = 0
				}
				continue
			}
			if c == '"' || c == '\'' {
				inQ = c
				continue
			}
			if c == '>' {
				inTag = false
			}
			continue
		}
		if c == '<' {
			inTag = true
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
}

// extractTables pulls every <table>…</table> segment out of the page.
func extractTables(doc string) []tableData {
	lower := strings.ToLower(doc)
	var tables []tableData
	pos := 0
	for {
		start := indexTag(lower, "table", pos)
		if start < 0 {
			break
		}
		rel := strings.Index(lower[start:], "</table")
		var seg string
		if rel < 0 {
			seg = doc[start:]
			pos = len(doc)
		} else {
			seg = doc[start : start+rel]
			pos = start + rel + len("</table")
		}
		tables = append(tables, parseTable(seg))
		if pos >= len(doc) {
			break
		}
	}
	return tables
}

func parseTable(seg string) tableData {
	lower := strings.ToLower(seg)
	var t tableData
	p := 0
	for {
		rStart := indexTag(lower, "tr", p)
		if rStart < 0 {
			break
		}
		rTagEnd := tagEnd(seg, rStart)
		next := indexTag(lower, "tr", rTagEnd+1)
		bodyEnd := len(seg)
		if next >= 0 {
			bodyEnd = next
		}
		row := tableRow{attrs: parseAttrs(seg[rStart : rTagEnd+1])}
		body := seg[rTagEnd+1 : bodyEnd]
		lowBody := lower[rTagEnd+1 : bodyEnd]

		c := 0
		for {
			iTD := indexTag(lowBody, "td", c)
			iTH := indexTag(lowBody, "th", c)
			cs, isHeader := iTD, false
			if cs < 0 || (iTH >= 0 && iTH < cs) {
				cs, isHeader = iTH, true
			}
			if cs < 0 {
				break
			}
			ce := tagEnd(body, cs)
			contentEnd := len(body)
			if n := indexTag(lowBody, "td", ce+1); n >= 0 && n < contentEnd {
				contentEnd = n
			}
			if n := indexTag(lowBody, "th", ce+1); n >= 0 && n < contentEnd {
				contentEnd = n
			}
			if n := strings.Index(lowBody[ce+1:], "</t"); n >= 0 && ce+1+n < contentEnd {
				contentEnd = ce + 1 + n
			}
			text := stripTags(body[ce+1 : contentEnd])
			row.cells = append(row.cells, tableCell{header: isHeader, text: text})
			if isHeader {
				t.headers = append(t.headers, strings.ToLower(text))
			}
			c = contentEnd
		}
		t.rows = append(t.rows, row)
		if next < 0 {
			break
		}
		p = next
	}
	return t
}

func headerIndex(headers []string, want string, exclude ...string) int {
	for i, h := range headers {
		if !strings.Contains(h, want) {
			continue
		}
		bad := false
		for _, x := range exclude {
			if strings.Contains(h, x) {
				bad = true
				break
			}
		}
		if !bad {
			return i
		}
	}
	return -1
}

// -------------------------------------------------------------- scraping

func fetchListings[T any](cacheName, url string, parse func(tableData) []T, useCache bool) ([]T, error) {
	cachePath := filepath.Join(dataDir(), cacheName)
	if useCache {
		if st, err := os.Stat(cachePath); err == nil && time.Since(st.ModTime()) < cacheTTL {
			if b, err := os.ReadFile(cachePath); err == nil {
				var out []T
				if json.Unmarshal(b, &out) == nil && len(out) > 0 {
					return out, nil
				}
			}
		}
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}

	// Every table goes through the parser; the one that yields the most
	// listings wins, so layout shuffles rarely break the scrape.
	var best []T
	for _, tbl := range extractTables(string(body)) {
		if got := parse(tbl); len(got) > len(best) {
			best = got
		}
	}
	if len(best) == 0 {
		return nil, errors.New("no parseable price table; the page layout may have changed")
	}
	if b, err := json.Marshal(best); err == nil {
		if os.WriteFile(cachePath, b, 0o644) == nil {
			fixOwner(cachePath)
		}
	}
	return best, nil
}

// parseDiskTable reads diskprices.com rows by header name first and the
// rows' data-* attributes second, so one layout change rarely breaks both.
func parseDiskTable(t tableData) []diskListing {
	iPrice := headerIndex(t.headers, "price", "per")
	iCap := headerIndex(t.headers, "capacity")
	iTech := headerIndex(t.headers, "technology")
	iCond := headerIndex(t.headers, "condition")

	var listings []diskListing
	for _, row := range t.rows {
		var cells []string
		for _, c := range row.cells {
			if !c.header {
				cells = append(cells, c.text)
			}
		}
		if len(cells) == 0 {
			continue
		}

		var price, capGB float64
		if iPrice >= 0 && iPrice < len(cells) {
			price = parsePrice(cells[iPrice])
		}
		if iCap >= 0 && iCap < len(cells) {
			capGB = parseCapacityGB(cells[iCap])
		}
		if price == 0 {
			price = parsePrice(row.attrs["data-price"])
		}
		if capGB == 0 {
			capGB = parseCapacityGB(row.attrs["data-capacity"] + " gb")
		}

		techText := ""
		if iTech >= 0 && iTech < len(cells) {
			techText = cells[iTech]
		}
		if techText == "" {
			techText = row.attrs["data-technology"]
		}
		if techText == "" {
			techText = strings.Join(cells, " ")
		}
		kind := classifyTech(techText)

		cond := ""
		if iCond >= 0 && iCond < len(cells) {
			cond = strings.ToLower(cells[iCond])
		} else if v, ok := row.attrs["data-condition"]; ok {
			cond = strings.ToLower(v)
		}
		if cond != "" && !strings.Contains(cond, "new") {
			continue
		}

		if kind != "" && price > 0 && capGB > 0 {
			listings = append(listings, diskListing{Kind: kind, GB: capGB, Price: price})
		}
	}
	return listings
}

// parseRAMTable reads ramstickprices.com rows. The site's Capacity column
// is per stick, but its Price-per-GB column divides by the kit's total,
// so price / price-per-GB recovers the total kit capacity. Rows for
// ancient MB-sized sticks carry garbage $/GB values (far below $0.50),
// and the parser drops them.
func parseRAMTable(t tableData) []ramListing {
	iPPG := headerIndex(t.headers, "per gb")
	iPrice := headerIndex(t.headers, "price", "per")
	if iPPG < 0 || iPrice < 0 {
		return nil
	}

	var listings []ramListing
	for _, row := range t.rows {
		var cells []string
		for _, c := range row.cells {
			if !c.header {
				cells = append(cells, c.text)
			}
		}
		if len(cells) <= iPPG || len(cells) <= iPrice {
			continue
		}
		price := parsePrice(cells[iPrice])
		ppg := parsePrice(cells[iPPG])
		if price == 0 || ppg < 0.5 {
			continue
		}
		totalGB := price / ppg
		if totalGB < 1 || totalGB > 2048 {
			continue
		}
		name := ""
		for _, c := range cells {
			if len(c) > len(name) {
				name = c
			}
		}
		gen := classifyRAMGeneration(name)
		if gen == "" {
			continue
		}
		listings = append(listings, ramListing{
			Gen:   gen,
			GB:    math.Round(totalGB*10) / 10,
			Price: price,
			ECC:   looksECC(name),
			Speed: parseSpeedMTS(name),
		})
	}
	return listings
}

// ------------------------------------------------------------- detection

func warn(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", a...)
}

func asString(v any) string { s, _ := v.(string); return s }

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	case float64:
		return t != 0
	}
	return false
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

func storageDevices() []component {
	out, err := exec.Command("lsblk", "-J", "-b", "-d", "-o",
		"NAME,TYPE,SIZE,ROTA,TRAN,MODEL").Output()
	if err != nil {
		warn("lsblk failed (%v); no storage detected", err)
		return nil
	}
	var doc struct {
		Blockdevices []map[string]any `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		warn("could not parse lsblk output: %v", err)
		return nil
	}
	var devs []component
	for _, d := range doc.Blockdevices {
		if asString(d["type"]) != "disk" {
			continue
		}
		name := asString(d["name"])
		skip := false
		for _, p := range []string{"loop", "zram", "ram", "sr"} {
			if strings.HasPrefix(name, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		size := asInt64(d["size"])
		if size < 1e9 { // skip sub-1GB devices (virtual disks, boot media)
			continue
		}
		tran := strings.ToLower(asString(d["tran"]))
		kind := "ssd"
		if tran == "nvme" {
			kind = "nvme"
		} else if asBool(d["rota"]) {
			kind = "hdd"
		}
		model := strings.TrimSpace(asString(d["model"]))
		if model == "" {
			model = "/dev/" + name
		}
		devs = append(devs, component{
			Key:   fmt.Sprintf("disk:%s:%d", model, size),
			Label: fmt.Sprintf("%s — %s %s", model, fmtSize(float64(size)), strings.ToUpper(kind)),
			Kind:  kind,
			Bytes: size,
		})
	}
	return devs
}

var (
	reDmiSize  = regexp.MustCompile(`(?m)^\s*Size:\s*(\d+)\s*([MG]B)`)
	reDmiType  = regexp.MustCompile(`(?m)^\s*Type:\s*(DDR\d)`)
	reDmiECC   = regexp.MustCompile(`(?m)^\s*Error Correction Type:\s*(.+)$`)
	reDmiSpeed = regexp.MustCompile(`(?m)^\s*Speed:\s*(\d+)\s*M`)
)

// ramComponent describes all installed RAM as one component. dmidecode
// (needs root) reports the true module total, DDR generation, ECC, and
// speed; /proc/meminfo is the fallback and knows only a size.
func ramComponent() component {
	var total int64
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) >= 2 {
					kb, _ := strconv.ParseInt(f[1], 10, 64)
					total = kb * 1024
				}
				break
			}
		}
	}

	gen := "unknown"
	ecc := false
	speed := 0
	var moduleTotal int64
	if out, err := exec.Command("dmidecode", "-t", "memory").Output(); err == nil {
		s := string(out)
		for _, m := range reDmiSize.FindAllStringSubmatch(s, -1) {
			n, _ := strconv.ParseInt(m[1], 10, 64)
			if m[2] == "MB" {
				moduleTotal += n << 20
			} else {
				moduleTotal += n << 30
			}
		}
		if m := reDmiType.FindStringSubmatch(s); m != nil {
			gen = strings.ToLower(m[1])
		}
		if m := reDmiECC.FindStringSubmatch(s); m != nil &&
			strings.Contains(strings.ToLower(m[1]), "ecc") {
			ecc = true
		}
		for _, m := range reDmiSpeed.FindAllStringSubmatch(s, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > speed {
				speed = n
			}
		}
	}

	nbytes := moduleTotal
	if nbytes == 0 {
		nbytes = total
	}
	gib := float64(nbytes) / (1 << 30)
	parts := []string{fmt.Sprintf("%.0f GiB", gib)}
	if gen != "unknown" {
		parts = append(parts, strings.ToUpper(gen))
	} else {
		parts = append(parts, "RAM")
	}
	if speed > 0 {
		parts = append(parts, fmt.Sprintf("%d MT/s", speed))
	}
	if ecc {
		parts = append(parts, "ECC")
	}
	return component{
		Key:   "ram",
		Label: strings.Join(parts, " "),
		Kind:  "ram",
		Gen:   gen,
		ECC:   ecc,
		Speed: speed,
		Bytes: nbytes,
	}
}

// --------------------------------------------------------------- pricing

type gp struct{ gb, price float64 }

func median(xs []float64) float64 {
	sort.Float64s(xs)
	n := len(xs)
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// medianNear takes the median price of listings whose capacity sits near
// the target, widening the window once before giving up.
func medianNear(pool []gp, target float64) (float64, int, bool) {
	for _, w := range [][2]float64{{0.9, 1.2}, {0.7, 1.6}} {
		var near []float64
		for _, l := range pool {
			if w[0]*target <= l.gb && l.gb <= w[1]*target {
				near = append(near, l.price)
			}
		}
		if len(near) >= 3 {
			return median(near), len(near), true
		}
	}
	return 0, 0, false
}

func medianPerGB(pool []gp) float64 {
	var rates []float64
	for _, l := range pool {
		if l.gb > 0 {
			rates = append(rates, l.price/l.gb)
		}
	}
	if len(rates) == 0 {
		return 0
	}
	return median(rates)
}

func priceStorage(listings []diskListing, kind string, nbytes int64,
	fallback map[string]float64) (float64, string) {
	gb := float64(nbytes) / 1e9
	var pool []gp
	for _, l := range listings {
		if l.Kind == kind {
			pool = append(pool, gp{l.GB, l.Price})
		}
	}
	if v, n, ok := medianNear(pool, gb); ok {
		return v, fmt.Sprintf("median of %d %s listings ~%s",
			n, strings.ToUpper(kind), fmtSize(float64(nbytes)))
	}
	if rate := medianPerGB(pool); rate > 0 {
		return rate * gb, fmt.Sprintf("market %s $/GB", strings.ToUpper(kind))
	}
	rate, ok := fallback[kind]
	if !ok {
		rate = 0.05
	}
	return rate * gb, "built-in $/GB estimate"
}

// priceRAM matches kits by generation first, then prefers the same ECC
// class and speed when enough such listings exist.
func priceRAM(listings []ramListing, ram component,
	fallback map[string]float64) (float64, string) {
	gib := float64(ram.Bytes) / (1 << 30)
	tag := strings.ToUpper(ram.Gen)
	if ram.ECC {
		tag += " ECC"
	}

	if ram.Gen == "unknown" {
		rate := fallback["unknown"]
		return rate * gib, fmt.Sprintf("generic $%.2f/GB (DDR generation unknown)", rate)
	}

	var byGen, byECC, bySpeed []gp
	for _, l := range listings {
		if l.Gen != ram.Gen {
			continue
		}
		byGen = append(byGen, gp{l.GB, l.Price})
		if l.ECC != ram.ECC {
			continue
		}
		byECC = append(byECC, gp{l.GB, l.Price})
		if ram.Speed > 0 && l.Speed == ram.Speed {
			bySpeed = append(bySpeed, gp{l.GB, l.Price})
		}
	}

	pools := []struct {
		pool []gp
		desc string
	}{
		{bySpeed, fmt.Sprintf("%s %d MT/s kits", tag, ram.Speed)},
		{byECC, tag + " kits"},
		{byGen, strings.ToUpper(ram.Gen) + " kits (any ECC)"},
	}
	for _, p := range pools {
		if v, n, ok := medianNear(p.pool, gib); ok {
			return v, fmt.Sprintf("median of %d %s ~%.0f GB", n, p.desc, gib)
		}
	}
	if rate := medianPerGB(byGen); rate > 0 {
		return rate * gib, fmt.Sprintf("market %s $/GB", strings.ToUpper(ram.Gen))
	}
	rate, ok := fallback[ram.Gen]
	if !ok {
		rate = fallback["unknown"]
	}
	return rate * gib, fmt.Sprintf("built-in %s $%.2f/GB", strings.ToUpper(ram.Gen), rate)
}

// -------------------------------------------------------------- main flow

func buildComponents(offline, noCache bool, ramPrice float64, cfg config) ([]component, []string) {
	notes := []string{}
	var diskListings []diskListing
	var ramListings []ramListing

	if !offline {
		var err error
		diskListings, err = fetchListings("disk_cache.json", diskURL, parseDiskTable, !noCache)
		if err != nil {
			notes = append(notes, fmt.Sprintf("disk price fetch failed (%v); using built-in rates", err))
		}
		ramListings, err = fetchListings("ram_cache.json", ramURL, parseRAMTable, !noCache)
		if err != nil {
			notes = append(notes, fmt.Sprintf("RAM price fetch failed (%v); using built-in rates", err))
		}
	}

	diskFallback := mergedRates(diskFallbackPerGB, cfg.DiskFallback)
	ramFallback := mergedRates(ramFallbackPerGB, cfg.RAMFallback)
	var components []component

	for _, dev := range storageDevices() {
		var value float64
		var basis string
		if len(diskListings) > 0 {
			value, basis = priceStorage(diskListings, dev.Kind, dev.Bytes, diskFallback)
		} else {
			rate, ok := diskFallback[dev.Kind]
			if !ok {
				rate = 0.05
			}
			value = rate * float64(dev.Bytes) / 1e9
			basis = "built-in $/GB estimate"
		}
		dev.Value = round2(value)
		dev.Basis = basis
		components = append(components, dev)
	}

	ram := ramComponent()
	if ram.Bytes > 0 {
		gib := float64(ram.Bytes) / (1 << 30)
		override := ramPrice
		if override == 0 {
			override = cfg.RAMPricePerGB
		}
		var value float64
		var basis string
		switch {
		case override > 0:
			value = override * gib
			basis = fmt.Sprintf("$%.2f/GB (your rate)", override)
		case len(ramListings) > 0:
			value, basis = priceRAM(ramListings, ram, ramFallback)
		default:
			rate, ok := ramFallback[ram.Gen]
			if !ok {
				rate = ramFallback["unknown"]
			}
			value = rate * gib
			basis = fmt.Sprintf("built-in $%.2f/GB", rate)
		}
		if ram.Gen == "unknown" && override == 0 {
			notes = append(notes, "DDR generation unknown (dmidecode needs root); "+
				"run with sudo for generation/ECC/speed matching")
		}
		ram.Value = round2(value)
		ram.Basis = basis
		components = append(components, ram)
	}
	return components, notes
}

func historyPath() string { return filepath.Join(dataDir(), "history.json") }

func loadHistory() []histEntry {
	b, err := os.ReadFile(historyPath())
	if err != nil {
		return nil
	}
	var h []histEntry
	if json.Unmarshal(b, &h) != nil {
		return nil
	}
	return h
}

func printReport(components []component, notes []string, prev *histEntry) {
	prevValues := map[string]float64{}
	if prev != nil {
		for _, c := range prev.Components {
			prevValues[c.Key] = c.Value
		}
	}

	type outRow struct{ label, val, change, basis string }
	var rows []outRow
	var total float64
	for _, c := range components {
		change := "—"
		if pv, ok := prevValues[c.Key]; ok {
			change = fmtDelta(c.Value - pv)
		} else if prev != nil {
			change = "new"
		}
		rows = append(rows, outRow{c.Label, fmtMoney(c.Value), change, c.Basis})
		total += c.Value
	}

	w0 := utf8.RuneCountInString("Component")
	w1 := utf8.RuneCountInString(fmtMoney(total))
	w2 := utf8.RuneCountInString("Change")
	for _, r := range rows {
		if n := utf8.RuneCountInString(r.label); n > w0 {
			w0 = n
		}
		if n := utf8.RuneCountInString(r.val); n > w1 {
			w1 = n
		}
		if n := utf8.RuneCountInString(r.change); n > w2 {
			w2 = n
		}
	}

	fmt.Printf("%s  %s  %s  Basis\n", padR("Component", w0), padL("Value", w1), padL("Change", w2))
	fmt.Printf("%s  %s  %s  %s\n", strings.Repeat("-", w0), strings.Repeat("-", w1),
		strings.Repeat("-", w2), strings.Repeat("-", 5))
	for _, r := range rows {
		fmt.Printf("%s  %s  %s  %s\n", padR(r.label, w0), padL(r.val, w1), padL(r.change, w2), r.basis)
	}
	fmt.Printf("%s  %s\n", strings.Repeat("-", w0), strings.Repeat("-", w1))
	if prev != nil {
		delta := total - prev.Total
		pct := 0.0
		if prev.Total != 0 {
			pct = delta / prev.Total * 100
		}
		fmt.Printf("%s  %s  %s (%+.1f%%) since %s\n", padR("Total", w0),
			padL(fmtMoney(total), w1), fmtDelta(delta), pct, prev.TS[:10])
	} else {
		fmt.Printf("%s  %s  first recorded run\n", padR("Total", w0), padL(fmtMoney(total), w1))
	}
	for _, n := range notes {
		fmt.Println("note: " + n)
	}
}

func main() {
	offline := flag.Bool("offline", false, "skip the network; use built-in $/GB estimates")
	jsonOut := flag.Bool("json", false, "print JSON instead of a table")
	dryRun := flag.Bool("dry-run", false, "don't record this run in history")
	noCache := flag.Bool("no-cache", false, "ignore the hourly price cache")
	ramPrice := flag.Float64("ram-price", 0, "skip RAM listings and use this price per GB")
	showHistory := flag.Bool("history", false, "show recorded totals and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "bananastand — estimate the market value of your RAM and drives")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("bananastand " + version)
		return
	}

	history := loadHistory()

	if *showHistory {
		if len(history) == 0 {
			fmt.Println("no runs recorded yet")
			return
		}
		var prevTotal *float64
		for _, e := range history {
			ts := e.TS
			if len(ts) >= 16 {
				ts = strings.Replace(ts[:16], "T", " ", 1)
			}
			line := fmt.Sprintf("%s  %s", ts, fmtMoney(e.Total))
			if prevTotal != nil {
				line += "  " + fmtDelta(e.Total-*prevTotal)
			}
			fmt.Println(line)
			t := e.Total
			prevTotal = &t
		}
		return
	}

	cfg := loadConfig()
	components, notes := buildComponents(*offline, *noCache, *ramPrice, cfg)
	if len(components) == 0 {
		fmt.Fprintln(os.Stderr, "no RAM or storage detected")
		os.Exit(1)
	}

	var prev *histEntry
	if len(history) > 0 {
		prev = &history[len(history)-1]
	}
	var total float64
	for _, c := range components {
		total += c.Value
	}
	total = round2(total)

	if *jsonOut {
		out := map[string]any{
			"generated":      time.Now().UTC().Format("2006-01-02T15:04:05-07:00"),
			"components":     components,
			"total":          total,
			"previous_total": nil,
			"change":         nil,
			"notes":          notes,
		}
		if prev != nil {
			out["previous_total"] = prev.Total
			out["change"] = round2(total - prev.Total)
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	} else {
		printReport(components, notes, prev)
	}

	if !*dryRun {
		entry := histEntry{
			TS:    time.Now().UTC().Format("2006-01-02T15:04:05-07:00"),
			Total: total,
		}
		for _, c := range components {
			entry.Components = append(entry.Components,
				histComponent{Key: c.Key, Label: c.Label, Value: c.Value})
		}
		history = append(history, entry)
		if b, err := json.MarshalIndent(history, "", "  "); err == nil {
			hp := historyPath()
			if err := os.WriteFile(hp, b, 0o644); err != nil {
				warn("could not save history: %v", err)
			} else {
				fixOwner(hp)
			}
		}
	}
}
