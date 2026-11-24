#!/bin/sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "Usage: sh $0 <scratch-image>" >&2
  exit 2
fi

image=$1
for io_args in "2 -n 7" "2 -n 0" "3"; do
  # The arguments are fixed above; splitting them supplies the ionice options.
  # shellcheck disable=SC2086
  docker run --rm --entrypoint /docker-entrypoint "$image" \
    nice -n 10 ionice -c $io_args restic version
done

if output=$(docker run --rm --cap-drop=ALL --ulimit nice=0:0 --entrypoint /docker-entrypoint \
  "$image" nice -n -10 restic version 2>&1); then
  echo "CPU_HIGH unexpectedly succeeded without CAP_SYS_NICE" >&2
  exit 1
fi

case "$output" in
  *"unable to set nice level"*) ;;
  *) echo "Missing priority adjustment error: $output" >&2; exit 1 ;;
esac
if printf '%s\n' "$output" | grep -Eq '^restic [0-9]'; then
  echo "Restic ran despite the priority adjustment failure: $output" >&2
  exit 1
fi
