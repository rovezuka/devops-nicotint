#!/usr/bin/env bash
# Скрытая поломка связности (Лаба 5, Часть 6): случайно выбирает файл из breakages/ и применяет его
# поверх values чарта через helm upgrade, НЕ показывая, какой выбран. Ответ — в .last-breakage (base64).
#   ./scripts/break.sh        — внести случайную поломку
#   ./scripts/break.sh fix    — вернуть политики без поломок
#   base64 -d .last-breakage  — посмотреть ответ (после диагностики!)
set -euo pipefail
cd "$(dirname "$0")/.."   # пути ниже относительно папки lab5

RELEASE="shop"
NAMESPACE="shop"
CHART="chart/shop"
BREAKAGES_DIR="breakages"
ANSWER_FILE=".last-breakage"

if [ "${1:-}" = "fix" ]; then
  # --reset-values обязателен: без -f/--set helm upgrade ПЕРЕИСПОЛЬЗУЕТ values прошлой ревизии,
  # т.е. повторно применил бы ту же поломку
  helm upgrade "$RELEASE" "$CHART" -n "$NAMESPACE" --reset-values
  rm -f "$ANSWER_FILE"
  echo "Политики восстановлены."
  exit 0
fi

if [ "$#" -gt 0 ]; then
  echo "Использование: $0 [fix]" >&2
  exit 1
fi

FILES=( "$BREAKAGES_DIR"/*.yaml )

if [ ! -f "${FILES[0]}" ]; then
  echo "Не найдены файлы поломок в $BREAKAGES_DIR/" >&2
  exit 1
fi

INDEX=$((RANDOM % ${#FILES[@]}))
SELECTED="${FILES[$INDEX]}"

# Сохраняем ответ в base64, не показывая его в терминале.
printf '%s' "$SELECTED" | base64 > "$ANSWER_FILE"

# Применяем выбранную поломку.
helm upgrade "$RELEASE" "$CHART" \
  -n "$NAMESPACE" \
  -f "$SELECTED"
