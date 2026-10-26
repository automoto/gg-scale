#!/bin/sh
set -eu

profile="${1:-coverage/coverage.out}"
module="$(go list -m)"
generated_files="${profile}.generated"
filtered_profile="${profile}.filtered"
trap 'rm -f "$generated_files" "$filtered_profile"' EXIT

find . -type f -name '*.go' ! -name '*_test.go' \
	-exec grep -l -m1 '^// Code generated .* DO NOT EDIT\.$' {} + > "$generated_files"

awk -v module="$module" -v generated_files="$generated_files" '
BEGIN {
	while ((getline file < generated_files) > 0) {
		sub(/^\.\//, "", file)
		generated[module "/" file] = 1
	}
	close(generated_files)
	prefix = module "/"
}
FNR == 1 {
	print
	next
}
{
	split($0, fields, ":")
	path = fields[1]
	if (path in generated) {
		next
	}
	if (index(path, prefix) == 1) {
		path = substr(path, length(prefix) + 1)
	}
	if (path ~ /^(scripts\/|examples\/|cmd\/openapi-dump\/|internal\/cache\/storetest\/|internal\/sso\/ssotest\/)/) {
		next
	}
	print
}
' "$profile" > "$filtered_profile"

mv "$filtered_profile" "$profile"
