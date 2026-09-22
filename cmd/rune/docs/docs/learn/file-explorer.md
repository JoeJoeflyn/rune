# File explorer

The file explorer is not a tree widget you click through. It is a live,
editable buffer of your workspace, and editing that buffer *is* how you
manage files. Rename a line and the file is renamed. Delete a line and
the file is gone. Re-indent a block and those files move into another
directory. You make all the changes you want as ordinary text edits,
then save once and confirm. It is the fastest way to do bulk file
surgery in Rune, and once it clicks you will not want to manage files
any other way.

<div className="docs-figure fexplorer-figure" role="img" aria-label="The Rune file explorer open as a floating window, showing the workspace as an editable buffer" />

## Opening it

The explorer is a pre-minimized floating window docked on the left. Open
it, focus it, and tuck it away again with a single binding:

<KeyBinding command="fexplorer" />

The first press un-minimizes the window and focuses it. Press it again
while the explorer is focused and it minimizes back out of the way. The
binding runs the `fexplorer` command, so you can remap it like any other
(see [layout management](./layout-management.md#rebinding)).

Rune remembers the window you came from. When you open a file from the
explorer, it lands in that window, not on top of the explorer.

In an empty workspace, or one where everything is ignored, the explorer
opens as an empty buffer rather than a tree. That is the expected
state: type the first file or directory name into it and save to create
it. `editor.file_explorer.min_width` sets how wide that empty split is.

## Moving around

The explorer renders the workspace as an indented tree inside a normal
editor buffer, so every editor motion you already know works here:
`hjkl` or the arrow keys, search, jumps, the lot.

| Key | On a file | On a directory |
| --- | --- | --- |
| `<enter>` | Opens the file in your last window | Expands or collapses the directory in place |

Expanding a directory reads its contents lazily and nests them under the
line, so you stay in one buffer instead of navigating away. There is no
separate "go up" command: the whole tree is right there, and you fold
directories closed when you want them out of sight.

## Editing is how you manage files

Here is the part that makes the explorer special. The buffer is the
source of truth, and Rune tracks the identity of every row, so it can
tell the difference between renaming a file and deleting one. That means
you can reshape your whole workspace with plain text edits:

- **Rename** a file or directory by editing its line.
- **Delete** a file by deleting its line (`dd` in modal mode).
- **Create** a file by typing a new line where you want it.
- **Create** a directory by adding a line for it and nesting children
  underneath.
- **Move** a file into another directory by changing its indentation.
  Indent a line one level deeper to push it *into* the directory above
  it; outdent it to move it back toward the parent.
- **Copy** a file by duplicating its line (yank and paste it elsewhere).

When you yank a row, you also yank the icon glyph rendered in front of
the name. Paste the name only, or fix the pasted line up so it reads
like the rest of the tree. Rune refuses to write a name containing an
icon glyph, and tells you which line is at fault.

None of this touches the disk while you type. You can stage as many
changes as you like, across as many directories as you like, and review
the whole batch before anything happens. This is what makes the explorer
a power tool for bulk work: rename a dozen files, reorganize a directory
tree, and fan out a new folder structure, all as one edit.

## Save, then confirm

When you are happy with the buffer, save it the way you save any file:

```
write
```

(`<esc>:w<enter>` in modal mode, or your usual save binding.) Saving
does not blindly apply your edits. Rune first diffs the buffer against
what is on disk, then shows you exactly what it is about to do and asks
for a yes/no confirmation:

> Apply the following changes?
>
> - **RENAME**  old-name -> new-name
> - **MOVE**    src/util.go -> internal/util.go
> - **DELETE**  scratch.txt
> - **CREATE**  notes/todo.md
> - **MKDIR**   notes

Press `y` to apply every change in one go, or `n` to back out and keep
your pending edits. If two lines would resolve to the same path, Rune
refuses the whole batch and tells you which path conflicts, so you never
half-apply a rename.

This save-and-confirm step is your safety net. You get the speed of bulk
text editing with a final, reviewable summary before a single file moves.

## What it hides

The explorer shows the same set of files the rest of Rune considers
worth seeing. It honors your workspace `.gitignore` and skips the usual
noise like `.git/` and editor swap files, so the tree stays focused on
the files you actually work with. File and directory icons match the
icon set the rest of the editor uses.

## Throwaway edits

Closing the explorer with the toggle while you have unsaved edits
discards them. Nothing is applied until you save and confirm, so if you
experiment in the buffer and change your mind, just toggle it shut and
start fresh next time.
