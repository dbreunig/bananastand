# bananastand

![There's always money in the banana stand](assets/theres-always-money-in-the-banana-stand.gif)

> There's always money in the banana stand.

bananastand detects your RAM and drives, prices them against live listings
(diskprices.com for drives, ramstickprices.com for RAM), and tells you how 
much your system is worth. RAM is matched by DDR generation, capacity, ECC,
and speed.

## Install

One line, on any Linux box (amd64 or arm64):

    curl -fsSL https://raw.githubusercontent.com/dbreunig/bananastand/main/install.sh | bash

## Usage

    bananastand                 # fetch prices, print values, record the run
    sudo bananastand            # root lets dmidecode report DDR gen/ECC/speed
    bananastand --history       # list recorded totals
    bananastand --dry-run       # don't record this run
    bananastand --json          # machine-readable output
    bananastand --offline       # no network; built-in $/GB rates
    bananastand --ram-price 8   # skip RAM listings; use this $/GB

Run it under sudo when you can — dmidecode needs root to report your DDR
generation, ECC, and speed, which is what makes RAM matching accurate.
Installed in /usr/local/bin, plain `sudo bananastand` just works. Sudo runs
still read and write *your* history (not root's) and hand file ownership
back to you, so plain and sudo runs share one record.

Config is optional, at `~/.config/bananastand/config.json`:

    {
      "ram_price_per_gb": 8.00,
      "fallback_price_per_gb": {"nvme": 0.07, "ssd": 0.06, "hdd": 0.018},
      "ram_fallback_price_per_gb": {"ddr4": 7.0, "ddr5": 11.0}
    }

History lives in `~/.local/share/bananastand/history.json`, in the same
format the earlier Python `sysvalue` tool used; to carry that history over,
copy `~/.local/share/sysvalue/history.json` into the new directory. Prices
are cached for an hour per source; `--no-cache` forces a refresh.

## Build from source

    go build              # current machine
    go test ./...         # parser and pricing tests
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build   # cross-compile
