#!/usr/bin/env bash
# Хаос подов (Лаба 5, Часть 2): раз в INTERVAL секунд удаляет случайный под api или worker в namespace shop.
# Запуск: ./chaos.sh [INTERVAL]   (по умолчанию 5 с), остановка — Ctrl+C.
set -u

NAMESPACE="shop"
INTERVAL="${1:-5}"

while true; do
  # bash 3.2 (macOS) не знает mapfile — разбиваем вывод на слова: имена подов без пробелов
  PODS=( $(kubectl get pods -n "$NAMESPACE" \
      -l 'app.kubernetes.io/component in (api,worker)' \
      -o name) )

  if [ "${#PODS[@]}" -eq 0 ]; then
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Нет подходящих подов"
    sleep "$INTERVAL"
    continue
  fi

  INDEX=$((RANDOM % ${#PODS[@]}))
  POD="${PODS[$INDEX]}"

  echo "[$(date '+%Y-%m-%d %H:%M:%S')] Удаляю $POD"

  kubectl delete -n "$NAMESPACE" "$POD" --wait=false

  sleep "$INTERVAL"
done
