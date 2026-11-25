# muhfm

My terminal file manager. One Go file, standard library only: no tcell, no
ncurses, no config framework, no plugins. `go build` gives one static binary
that does what Dolphin does, in a terminal.

```sh
go build -o ~/.local/bin/muhfm .   # Go 1.22+, nothing to download
muhfm --set-default                # folders now open in muhfm, from any app
```

## What it does

| | |
|---|---|
| **browse** | details view (size, item counts, dates), natural sort (2 before 10), sort by name/size/date/extension, folders first, hidden files, live refresh when a folder changes |
| **places** | sidebar with home, XDG folders, root, trash, bookmarks and drives; `'` jumps anywhere with type-to-filter |
| **tabs and split** | `ctrl+t` tabs, `\|` two panes, F6/F7 copy/move to the other pane, back/forward history, recent folders, marks |
| **files** | copy, cut, paste, paste as symlink, with progress, cancel, and a keep-both/overwrite/skip choice on conflicts; rename; bulk rename in `$EDITOR`; new file/folder (nested paths ok); chmod; undo for all of it |
| **trash** | freedesktop trash (shared with every other file manager): browse, restore, delete, empty; per-drive `.Trash-$UID` on USB sticks |
| **search** | filter as you type, recursive find by name (glob ok), find by content, "show in folder" |
| **open** | default apps from your desktop entries and `mimeapps.list`, open with… any app, make it the default, terminal apps run in place, `$EDITOR`, terminal or shell here, run commands on the selection |
| **preview** | text, folders, archives, PDFs (with `pdftotext`), hex for binaries, real images on sixel terminals (foot) |
| **archives** | list, extract (zip, tar, tar.gz, tar.bz2, gz built in; 7z/rar/xz/zst via bsdtar), compress to zip/tar/tar.gz |
| **drives** | mount, unmount and eject USB drives (udisks2) |
| **properties** | type, size of a whole folder, permissions, owner, dates, inode, free space, default app, MD5/SHA-1/SHA-256 |
| **desktop** | system clipboard both ways (copy in muhfm, paste in a GUI app and back), GTK bookmarks, "show in folder" from browsers, a file picker mode for scripts |
| **mouse** | click, double-click, right-click menu, wheel, click the sidebar and tabs |

Press `?` inside muhfm, or run `muhfm --keys`, for every key. The key table in
`main.go` is the only list of keys; help and `--keys` are generated from it.

## Default file manager

`muhfm --set-default` writes `~/.local/share/applications/muhfm.desktop` and
sets it as the default for `inode/directory` in `~/.config/mimeapps.list`.
The desktop entry runs `muhfm --window`, which opens a terminal (the
`terminal` setting, default `foot`) when it was not started from one. So
"Open folder" in a browser, `xdg-open ~/Downloads` and GTK apps all land in
muhfm, and a file argument opens its folder with that file selected.

## Shell integration

cd to wherever you quit:

```sh
fm() { f=$(mktemp); muhfm --choosedir="$f" "$@"; d=$(cat "$f"); rm -f "$f"; [ -d "$d" ] && cd "$d"; }
```

Pick files from a script: `muhfm --pick` prints the chosen paths, or
`--choosefiles=FILE` writes them to a file.

## Config

Optional. `~/.config/muhfm/config`, plain `key = value`:

```
hidden = false        # show dotfiles
preview = true
places = true         # sidebar
details = true
sort = name           # name | size | time | ext
reverse = false
dirs_first = true
icons = none          # none | nerd | emoji | ascii
sixel = auto          # auto | yes | no
terminal = foot       # used for --window and T
editor = nvim         # default: $VISUAL, $EDITOR, vi
```

Colours are the terminal's 16 ANSI colours, so muhfm follows whatever theme
your terminal uses. In [Rayconfig](https://github.com/apv022/Rayconfig) that
is the desktop palette.

## Optional helpers

None are required. When present they add one thing each: `wl-copy`/`wl-paste`
for the system clipboard, `udisksctl` for drives, `lsblk` to list them,
`bsdtar` for exotic archives, `pdftotext` for PDF previews, `xdg-open` as a
last resort for files nothing claims.

## Limits

Linux only (it talks to the terminal directly). No drag and drop: it is a
terminal. Image previews need a sixel terminal; elsewhere you get the image
size and type.
