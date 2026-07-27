# Troubleshooting

The first stop for any problem is **`http://<host>:7979/diag`** (log in
first): it shows the version, which sections are configured and where each
setting came from, whether your download clients are reachable, poller
health, and the last recovered crash. Secrets are masked. Paste it into a
bug report.

The three mistakes below cause almost every "it's configured but nothing
works" report. All three share a symptom: everything *looks* fine —
searches work, grabs start, downloads finish — and then nothing lands in
the library.

## 1. Wrong container path (Docker)

Every path forage sees is a path **inside its container**. If your compose
file mounts `/mnt/media` at `/data`, then the download folder is
`/data/downloads/complete` and the library is `/data/library` — never
`/mnt/media/...`, which doesn't exist inside the container.

Telltales:

- Settings → Test library fails, or placement silently never happens.
- `/diag` shows `placerConfigured: false` despite a library root being set.
- Logs mention `no such file or directory` on a path that exists on the
  host.

Fix: use the in-container path everywhere in the wizard/Settings. One
volume, one path, both folders inside it.

## 2. Download folder and library on different filesystems

forage places by **hardlink**, which only works within one filesystem. On
different disks (or different Docker volumes — two binds of the same disk
are fine, two different volumes are not) it falls back to copying: slower,
double the space, and the torrent seeds from a file you're also storing a
copy of.

Telltales:

- Placement works but is slow and disk usage doubles.
- Logs say `falling back to copy` / `cross-device`.

Fix: put `downloads/` and `library/` under the same mount, and bind-mount
that one root into the container at one path (the compose template's
one-volume layout).

## 3. Download client category pointing somewhere else

forage only places files from its own category (`forage` by default), and
it needs that category's save path to be the download folder it knows
about. Saving the Download clients section creates or **repoints** the
category automatically — but if you later change the category's save path
in qBittorrent/SABnzbd itself, or point forage's download folder
somewhere else, finished files land where forage isn't looking.

Telltales:

- Grabs sit at `completed` and never become `placed`.
- The finished file is on disk, but under a path other than
  `<download folder>/...`.

Fix: re-save the Download clients section in Settings (it repoints the
category), or align the category's save path with forage's download
folder.

## Other quick answers

**Grabs stuck at `downloading` with a reachable qBittorrent** — check
`/healthz` `clientErrors`; a stalled torrent with no seeders is qBit's to
report, and forage will orphan it after the configured window rather than
guess.

**A scene was deleted and you want it back** — Deletions page (or
`GET /destructions`). `trashed` entries have a Restore button for the
retention window (7 days by default). Every deletion forage performed or
refused is journalled there, with the full file list.

**The daemon seems idle** — `/healthz` `poller.lastTickAt` should move
every poll interval; `libraryOk: false` means the daemon can't see the
library mount and has paused placement and refused destroys on purpose.
`lastPanic` means a background loop crashed and recovered — `/diag` has
the stack; please file it.
