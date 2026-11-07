#!/bin/sh
# Запускает go test. Если Go установлен — напрямую, иначе в контейнере: на
# машине разработчика (Windows) Go нет, а тесты обязаны идти на Linux, потому
# что часть кода (SO_MARK, SO_ORIGINAL_DST) собирается только под ним.
set -eu
cd "$(dirname "$0")/.."
if command -v go >/dev/null 2>&1; then
	exec go test "$@"
fi
# pwd -W — путь в формате Windows для Git Bash; на Linux такого ключа нет.
root=$(pwd -W 2>/dev/null || pwd)
# Без этого Git Bash переписывает пути внутри аргументов docker.
export MSYS_NO_PATHCONV=1
exec docker run --rm -v "$root:/src" -w /src golang:1.27.1 go test "$@"
