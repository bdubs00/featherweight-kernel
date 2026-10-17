#!/bin/sh
# Print the commits since a tag: hash, date, and subject, one per line.
set -e
since="${1:?usage: commits.sh <tag>}"
git log --no-merges --date=short --pretty='%h %ad %s' "${since}..HEAD"
