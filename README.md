# VM IOPS Manager

English | [Русский](README.ru.md)

A tool for managing the IOPS limit on virtual machine disks in vCenter 8,
where this capability was removed from the UI. 
Replace for [PowerShell/WinForms-script](https://github.com/Alexaldonov/vSphere-8-IOPS-Manager): 
it's a single exe (~14 MB, no PowerCLI needed), starts in under a second, and its interface is a web
page that opens in the default browser.
The functionality has been expanded compared to my old utility.
Also compatible with vCenter 7.

## How it works

1. `vm-iops-manager.exe` starts an HTTP server bound to `127.0.0.1` only, on
   a random free port, and opens the default browser at
   `http://127.0.0.1:<port>/?token=<one-time token>`.
2. Every API request requires that token (the `X-Auth-Token` header), and
   the server also checks the `Host` header. This matters on a shared
   terminal server: a request without the token, or with a foreign `Host`,
   is rejected (403), so other users logged into the same machine can't
   reach your local server or your vCenter session.
3. If you close the tab/browser, just open the link from the console again,
   or restart the exe (a new port and token are generated each time).
4. One running instance can hold several vCenter connections at once, and
   you switch between them by clicking a "pill" in the top bar — no need to
   reconnect.
5. As usual, a vCenter session expires after a period of inactivity
   (typically ~30 minutes). As soon as that happens, the first action that
   hits the dead session returns an error — the tool drops the stale
   connection itself and opens the connect form with the address and
   username already filled in; only the password needs to be re-entered.

## Running it

Just run `vm-iops-manager.exe`. A console window opens with the address (in
case the browser doesn't open on its own) along with the browser UI.
Closing the console window stops the server.

No installation, PowerShell, or PowerCLI is required — all the vCenter
request code ([govmomi](https://github.com/vmware/govmomi), VMware's official Go library) is embedded in the exe.

## Interface features

- UI in Russian or English — a language switch (RU/EN) in the top-right
  corner, with the choice remembered in the browser. It defaults to the
  browser's own language.
- Connect to several vCenters at once, and switch between them quickly
  without re-entering a password.
- Search VMs by a case-insensitive name substring, or by an exact VM ID
  (moref, e.g. `vm-1234`).
- A disk table for the found VM showing the current IOPS limit, the
  SCSI/SATA/NVMe address in the familiar `(bus:unit)` form (e.g.
  `Hard disk 1 (0:0)`), and the controller type (LSI Logic SAS, VMware
  Paravirtual, NVMe, etc.) — the same information vSphere Client shows when
  a disk is selected.
- Edit the limit of a single disk (`-1` = unlimited).
- A "Reset all limits" button — clears the limits on all of a VM's disks in
  one `ReconfigVM_Task` call (all disks are submitted in a single request).
- A "Resource Pool" tab: search resource pools by a name substring (shown
  with their owning cluster/host, to tell same-named pools apart), view the
  VM list, and a "Reset limits for all VMs" button for a bulk reset across
  the whole pool. **Only VMs directly inside the pool are included** — VMs
  in nested resource pools are excluded and not affected (this is
  intentional: otherwise accidentally selecting a top-level pool could hit
  thousands of VMs in its nested pools). Each VM is processed with its own
  `ReconfigVM_Task` (up to 3 at a time, to avoid a load spike on ESXi); a
  summary and a list of any VMs where the reset failed are shown afterward.
- A "Datastore" tab: search datastores by a name substring, view a table of
  VMs and just the disks that live on that particular datastore (the same
  VM's disks on other datastores are excluded from the table), and a
  "Reset limits on this datastore" button for a targeted reset of only
  those disks. Use case: you need to put a datastore into maintenance mode
  urgently, and clearing the limits speeds up Storage vMotion for exactly
  its disks, without touching the same VMs' other disks. The VM list is
  built from the datastore's own `vm` property (every VM that has at least
  one file on it), so it doesn't require scanning every VM in vCenter.
- Action log: every limit change (who — by connection login, what exactly,
  and when) is printed line by line to the exe's console window. The tool
  keeps no history or database of its own — it's just a stdout stream;
  watch it directly or redirect it to a file if needed
  (`vm-iops-manager.exe > log.txt 2>&1`, or `... | tee log.txt` if you want
  both the screen and a file).
- A "File check" tab: paste a list of `.vmx`/`.vmdk` paths (one per line)
  and get, for each, a status: in use (with the VM's name) or **ZOMBIE** —
  not registered to any VM. This is PowerCLI commands analogue
  `Get-VM | Get-HardDisk | Where-Object { $_.Filename -eq "..." }` for vmdk
  and `Get-VM | Where-Object { $_.ExtensionData.Config.Files.VmPathName -eq
  "..." }` for vmx, but for the whole list at once: the VM inventory is
  scanned once (building a path index), then every submitted path is
  checked against that index — not with a fresh scan per path. Comparison
  is case-insensitive, matching PowerShell's `-eq`. Limitation: for vmdk,
  only the current (topmost) file in the snapshot chain is considered, the
  same as `Get-HardDisk`'s `$_.Filename` without walking the delta-file
  chain.
  Separately, the tool checks whether the datastore named in `[name]`
  actually exists in this vCenter — if not (say, the report was made
  against a different vCenter, or the datastore was decommissioned long
  ago), the path is flagged "datastore not found in this vCenter" instead
  of being wrongly reported as a zombie.
  If the datastore is found, the tool additionally checks whether the file
  itself physically exists on it (via `HostDatastoreBrowser`, the same
  mechanism vSphere Client uses to browse datastore files). If it's not
  there, the path is flagged "file not found on datastore" (most likely a
  typo in the path, or the file was already deleted); only when the file
  physically exists but isn't registered to any VM is it reported as a
  ZOMBIE.

## Technical details

- Language: Go. The vCenter library is `govmomi` (VMware/Broadcom's
  official Go SDK).
- Passwords are never stored anywhere — only used at connect time; the
  session lives in the running process's memory.
- By default, connections ignore invalid SSL certificates (equivalent to
  `Set-PowerCLIConfiguration -InvalidCertificateAction Ignore` in the
  original script) — uncheck the box in the connect form if you need strict
  validation.
- Every limit change is a separate `ReconfigVM_Task` call, awaited
  synchronously before the result is shown.

## Building from source

Requires Go 1.24+ (or `GOTOOLCHAIN=auto`, which lets Go fetch the right
version on its own):

```
go mod tidy
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o vm-iops-manager.exe .
```

Files:
- `main.go` — starts the HTTP server, generates the token, opens the browser.
- `vcenter.go` — all the vCenter logic via govmomi (connecting, searching
  VMs/pools/datastores, reading and changing disk limits, the action log
  via `logAction`).
- `api.go` — the REST API on top of `vcenter.go`.
- `web/index.html` — the entire UI (HTML/CSS/JS in one file), embedded into
  the exe via `go:embed`.

## Possible future work

- An indicator in search results showing that a VM currently has its
  limit(s) cleared — could check `StorageIOAllocation.Limit == -1` across
  all disks and highlight the row.
- A cluster/resource-pool/datastore filter, in case a name-substring search
  starts returning too many matches.
- A "show shares/reservation" button — the API already has all the data at
  the `StorageIOAllocation` level; it's deliberately not surfaced in the
  current version (not needed for the working scenario of temporarily
  clearing limits).

## License

[MIT](LICENSE).
