---
name: release-notes
description: Write the release notes of a repository from its git history since the last tag.
---

# Release notes

Use this skill when the task asks for release notes, a changelog entry, or a summary of what changed since a release.

## Steps

1. Find the last tag with `git describe --tags --abbrev=0`. If there is no tag, use the first commit.
2. List the commits since the tag with `scripts/commits.sh <tag>`. The script prints one line per commit: the hash, the date, and the subject.
3. Group the commits by the area they touch. Read a commit with `git show --stat <hash>` when the subject does not say what changed.
4. Write one section per area, with one sentence per change. Name the user-facing effect, not the file.
5. Put breaking changes first, under their own heading.

## Rules

- Do not invent a change that the history does not show.
- Do not list merge commits or version bumps.
- Keep the notes under 400 words.
