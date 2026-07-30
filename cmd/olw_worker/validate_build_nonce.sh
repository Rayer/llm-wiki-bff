#!/bin/sh

set -eu

if [ "$#" -ne 1 ]; then
	echo "BUILD_NONCE must be provided as a single argument" >&2
	exit 1
fi

value=$1
if [ -z "${value}" ] || [ "${#value}" -ne 32 ]; then
	echo "BUILD_NONCE must be 32 lowercase hexadecimal characters" >&2
	exit 1
fi

case "${value}" in
	*[!0-9a-f]*|"")
		echo "BUILD_NONCE must be 32 lowercase hexadecimal characters" >&2
		exit 1
		;;
esac

exit 0
