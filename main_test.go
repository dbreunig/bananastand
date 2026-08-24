package main

import (
	"math"
	"testing"
)

// Fixture modeled on the real diskprices.com table.
const diskFixture = `
<html><body><table id="diskprices">
<thead><tr><th>Price</th><th>Price per GB</th><th>Capacity</th><th>Warranty</th>
<th>Form Factor</th><th>Technology</th><th>Condition</th><th>Name</th></tr></thead>
<tbody>
<tr><td>$59.99</td><td>$0.060</td><td>1 TB</td><td>5y</td><td>M.2</td><td>NVMe</td><td>New</td><td><a href="#">Brand X 1TB</a></td></tr>
<tr><td>$64.50</td><td>$0.064</td><td>1 TB</td><td>5y</td><td>M.2</td><td>NVMe</td><td>New</td><td>Brand Y 1TB</td></tr>
<tr><td>$55.00</td><td>$0.055</td><td>1 TB</td><td>3y</td><td>M.2</td><td>NVMe</td><td>New</td><td>Brand Z 1TB</td></tr>
<tr><td>$40.00</td><td>$0.040</td><td>960 GB</td><td>3y</td><td>M.2</td><td>NVMe</td><td>Used</td><td>Used drive</td></tr>
<tr><td>$129.99</td><td>$0.016</td><td>8 TB</td><td>2y</td><td>3.5&quot;</td><td>HDD (7200 RPM)</td><td>New</td><td>Big HDD</td></tr>
</tbody></table></body></html>`

// Fixture modeled on the real ramstickprices.com table: Capacity is per
// stick, Price per GB divides by the kit total, and MB-era rows carry
// garbage $/GB values.
const ramFixture = `
<html><body><table>
<thead><tr><th>Price per GB</th><th>Price</th><th>Capacity</th><th>Affiliate Link</th></tr></thead>
<tbody>
<tr><td>7.50</td><td>$240.00</td><td>16</td><td><a href="#">Corsair Vengeance 32GB (2x16GB) DDR5 5600MHz CL36 Desktop Memory</a></td></tr>
<tr><td>7.19</td><td>$230.00</td><td>16</td><td><a href="#">G.SKILL 32GB (2x16GB) DDR5 6000MT/s Non-ECC Kit</a></td></tr>
<tr><td>7.81</td><td>$249.99</td><td>32</td><td><a href="#">Kingston Fury 32GB DDR5 5200MT/s (Kit of 2)</a></td></tr>
<tr><td>6.50</td><td>$104.00</td><td>8</td><td><a href="#">TEAMGROUP 16GB (2x8GB) DDR4 3200MHz CL16 UDIMM</a></td></tr>
<tr><td>2.50</td><td>$80.00</td><td>16</td><td><a href="#">NEMIX 32GB (2X16GB) DDR3 1600MHZ PC3-12800 ECC RDIMM Registered Server Memory</a></td></tr>
<tr><td>0.02</td><td>$10.49</td><td>512</td><td><a href="#">Kingston 512MB 533MHz DDR2 NonECC CL4 DIMM Memory</a></td></tr>
</tbody></table></body></html>`

func bestDiskParse(doc string) []diskListing {
	var best []diskListing
	for _, t := range extractTables(doc) {
		if got := parseDiskTable(t); len(got) > len(best) {
			best = got
		}
	}
	return best
}

func bestRAMParse(doc string) []ramListing {
	var best []ramListing
	for _, t := range extractTables(doc) {
		if got := parseRAMTable(t); len(got) > len(best) {
			best = got
		}
	}
	return best
}

func TestParseDiskTable(t *testing.T) {
	rows := bestDiskParse(diskFixture)
	if len(rows) != 4 { // used drive excluded
		t.Fatalf("want 4 rows, got %d: %+v", len(rows), rows)
	}
	v, basis := priceStorage(rows, "nvme", 1_000_000_000_000, diskFallbackPerGB)
	if v != 59.99 { // median of the three new 1TB NVMe listings
		t.Fatalf("want 59.99, got %v (%s)", v, basis)
	}
	if _, basis := priceStorage(rows, "hdd", 8_000_000_000_000, diskFallbackPerGB); basis == "" {
		t.Fatal("hdd pricing returned no basis")
	}
}

func TestParseRAMTable(t *testing.T) {
	rows := bestRAMParse(ramFixture)
	if len(rows) != 5 { // broken 512MB row dropped
		t.Fatalf("want 5 rows, got %d: %+v", len(rows), rows)
	}
	var ddr5 []ramListing
	for _, r := range rows {
		if r.Gen == "ddr5" {
			ddr5 = append(ddr5, r)
		}
	}
	if len(ddr5) != 3 {
		t.Fatalf("want 3 DDR5 rows, got %+v", ddr5)
	}
	for _, r := range ddr5 {
		if r.GB < 31 || r.GB > 33 {
			t.Fatalf("DDR5 kit total should be ~32 GB, got %+v", r)
		}
	}
	if ddr5[0].Speed != 5600 || ddr5[0].ECC {
		t.Fatalf("first DDR5 row misparsed: %+v", ddr5[0])
	}
	if !rows[4].ECC {
		t.Fatalf("RDIMM row should be flagged ECC: %+v", rows[4])
	}
}

func TestPriceRAM(t *testing.T) {
	rows := bestRAMParse(ramFixture)

	// 32 GiB DDR5 5600 non-ECC: not enough speed matches, so the ECC-class
	// pool wins: median of the three DDR5 kits = $240.
	ram := component{Bytes: 32 << 30, Gen: "ddr5", ECC: false, Speed: 5600}
	v, basis := priceRAM(rows, ram, ramFallbackPerGB)
	if v != 240.00 {
		t.Fatalf("want 240.00, got %v (%s)", v, basis)
	}

	// 32 GiB DDR3 ECC: one exact listing isn't enough for a median, so it
	// prices from the DDR3 market $/GB: 2.50 * 32 = 80.
	ram = component{Bytes: 32 << 30, Gen: "ddr3", ECC: true, Speed: 1600}
	v, basis = priceRAM(rows, ram, ramFallbackPerGB)
	if math.Abs(v-80.0) > 0.5 {
		t.Fatalf("want ~80, got %v (%s)", v, basis)
	}

	// Unknown generation: generic fallback rate, no bogus matching.
	ram = component{Bytes: 16 << 30, Gen: "unknown"}
	v, _ = priceRAM(rows, ram, ramFallbackPerGB)
	if v != 16*ramFallbackPerGB["unknown"] {
		t.Fatalf("want %v, got %v", 16*ramFallbackPerGB["unknown"], v)
	}
}

func TestFormatting(t *testing.T) {
	if got := fmtMoney(1234567.891); got != "$1,234,567.89" {
		t.Fatalf("fmtMoney: %s", got)
	}
	if got := fmtDelta(-3.5); got != "-$3.50" {
		t.Fatalf("fmtDelta: %s", got)
	}
	if got := fmtSize(1.5e12); got != "1.5 TB" {
		t.Fatalf("fmtSize: %s", got)
	}
	if got := stripTags(`a <b class=">">bold&amp;</b> c`); got != "a bold& c" {
		t.Fatalf("stripTags: %q", got)
	}
}
