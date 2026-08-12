#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 check BUCKETS -- PACKAGES... | pattern BUCKET BUCKETS -- PACKAGES..." >&2
  exit 2
}

mode=${1:-}
case "$mode" in
  check)
    configured=${2:-}
    shift 2
    ;;
  pattern)
    selected=${2:-}
    configured=${3:-}
    shift 3
    ;;
  *) usage ;;
esac

[[ ${1:-} == -- && -n "$configured" ]] || usage
shift
[[ $# -gt 0 ]] || usage

records=$(mktemp)
trap 'rm -f "$records"' EXIT

for package in "$@"; do
  directory=${package#./}
  directory=${directory%/...}
  while IFS= read -r -d '' file; do
    tests=$(awk '
      /^func Test[[:alnum:]_]*\(/ {
        name = $2
        sub(/\(.*/, "", name)
        if (name != "TestMain") print name
      }
    ' "$file")
    [[ -n "$tests" ]] || continue

    tags=$(awk '$1 == "//" && $2 == "e2e:bucket" && NF == 3 { print $3 }' "$file")
    tag_count=$(printf '%s\n' "$tags" | awk 'NF { count++ } END { print count + 0 }')
    if [[ $tag_count -ne 1 ]]; then
      echo "$file: expected exactly one '// e2e:bucket LETTER' tag" >&2
      exit 1
    fi
    case ",$configured," in
      *",$tags,"*) ;;
      *) echo "$file: unknown e2e bucket '$tags'" >&2; exit 1 ;;
    esac

    while IFS= read -r test; do
      printf '%s\t%s\n' "$tags" "$test" >>"$records"
    done <<<"$tests"
  done < <(find "$directory" -type f -name '*_test.go' -print0)
done

conflict=$(LC_ALL=C sort -u -k2,2 -k1,1 "$records" | awk '
  previous_test == $2 && previous_bucket != $1 { print $2; exit }
  { previous_bucket = $1; previous_test = $2 }
')
if [[ -n "$conflict" ]]; then
  echo "top-level test name '$conflict' occurs in multiple buckets" >&2
  exit 1
fi

if [[ $mode == pattern ]]; then
  case ",$configured," in
    *",$selected,"*) ;;
    *) echo "unknown e2e bucket '$selected'" >&2; exit 1 ;;
  esac
  pattern=$(awk -v bucket="$selected" '$1 == bucket { print $2 }' "$records" | LC_ALL=C sort -u | paste -sd '|' -)
  [[ -n "$pattern" ]] || { echo "e2e bucket '$selected' is empty" >&2; exit 1; }
  printf '^(%s)$\n' "$pattern"
  exit
fi

summary=""
for bucket in ${configured//,/ }; do
  count=$(awk -v bucket="$bucket" '$1 == bucket { count++ } END { print count + 0 }' "$records")
  [[ $count -gt 0 ]] || { echo "e2e bucket '$bucket' is empty" >&2; exit 1; }
  summary="${summary}${summary:+, }${bucket}=${count}"
done
echo "end-to-end source tags cover $(wc -l <"$records" | tr -d ' ') tests exactly once ($summary)"
