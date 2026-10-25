#!/usr/bin/env bash
# Проверка матрицы доступа (Лаба 5, Часть 5).
# Из network namespace пода-источника (nsenter на его ноде) пытается открыть TCP-соединение к назначению.
# Пакет уходит через lxc-интерфейс пода, поэтому Cilium применяет identity и политики именно этого пода.
#
#   exit 0   → соединение установлено        → ALLOW
#   exit 124 → таймаут: пакет молча отброшен → DENY (так Cilium ведёт себя при запрете)
#   exit 1   → connection refused: пакет дошёл, но порт закрыт → REFUSED (не политика)
#
# Запуск: ./scripts/matrix.sh            (профиль minikube — lab5)
set -u
PROFILE="${PROFILE:-lab5}"
TIMEOUT=3

# netns-PID пода: берём pause-контейнер (k8s_POD_<pod>_<ns>_…) — он держит сетевой namespace пода
pod_pid() { # $1=ns $2=pod $3=node
  minikube -p "$PROFILE" ssh -n "$3" -- \
    "docker inspect -f '{{.State.Pid}}' \$(docker ps -q --filter name=k8s_POD_${2}_${1}_ | head -1)" 2>/dev/null | tr -d '\r'
}

first_pod() { # $1=ns $2=selector → "pod node"
  kubectl get pods -n "$1" -l "$2" --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name} {.items[0].spec.nodeName}'
}

pod_ip() { kubectl get pod -n "$1" "$2" -o jsonpath='{.status.podIP}'; }

check() { # $1=src_ns $2=src_selector $3=dst_ip $4=port → ALLOW|DENY|REFUSED
  read -r POD NODE <<<"$(first_pod "$1" "$2")"
  local PID; PID=$(pod_pid "$1" "$POD" "$NODE")
  minikube -p "$PROFILE" ssh -n "$NODE" -- \
    "sudo nsenter -t $PID -n timeout $TIMEOUT bash -c '</dev/tcp/$3/$4' 2>/dev/null; echo \$?" 2>/dev/null | tr -d '\r' | tail -1
}

verdict() { case "$1" in 0) echo ALLOW;; 124) echo DENY;; 1) echo REFUSED;; *) echo "ERR($1)";; esac; }

# --- адреса назначений ---
API_SVC=$(kubectl get svc shop-api -n shop -o jsonpath='{.spec.clusterIP}')
read -r WORKER_POD _ <<<"$(first_pod shop app.kubernetes.io/component=worker)"; WORKER_IP=$(pod_ip shop "$WORKER_POD")
PG_IP=$(pod_ip shop shop-db-1)
read -r DNS_POD _ <<<"$(first_pod kube-system k8s-app=kube-dns)"; DNS_IP=$(pod_ip kube-system "$DNS_POD")
K8S_API=$(kubectl get svc kubernetes -n default -o jsonpath='{.spec.clusterIP}')

# источник(ns|selector|подпись) ; назначение(ip|порт|подпись) ; ожидание
ROWS=(
  "shop|app.kubernetes.io/component=loadgen|loadgen;$API_SVC|80|api (Service);ALLOW"
  "default|app.kubernetes.io/name=stranger|посторонний (default);$API_SVC|80|api (Service);ALLOW"
  "shop|app.kubernetes.io/component=api|api;$PG_IP|5432|postgres;ALLOW"
  "shop|app.kubernetes.io/component=worker|worker;$PG_IP|5432|postgres;ALLOW"
  "shop|app.kubernetes.io/component=loadgen|loadgen;$PG_IP|5432|postgres;DENY"
  "default|app.kubernetes.io/name=stranger|посторонний (default);$PG_IP|5432|postgres;DENY"
  "default|app.kubernetes.io/name=stranger|посторонний (default);$PG_IP|8000|postgres (статус);DENY"
  "shop|app.kubernetes.io/component=api|api;$WORKER_IP|8080|worker;DENY"
  "shop|app.kubernetes.io/component=worker|worker;$API_SVC|80|api (Service);DENY"
  "monitoring|app.kubernetes.io/name=prometheus|Prometheus;$WORKER_IP|8080|worker (metrics);ALLOW"
  "monitoring|app.kubernetes.io/name=grafana|Grafana (monitoring);$WORKER_IP|8080|worker (metrics);DENY"
  "default|app.kubernetes.io/name=stranger|посторонний (default);$WORKER_IP|8080|worker (metrics);DENY"
  "cnpg-system|app.kubernetes.io/name=cloudnative-pg|оператор CNPG;$PG_IP|8000|postgres (статус);ALLOW"
  "shop|app.kubernetes.io/component=api|api;$DNS_IP|53|CoreDNS (TCP);ALLOW"
  "shop|cnpg.io/cluster=shop-db|postgres;$K8S_API|443|Kubernetes API;ALLOW"
  "shop|app.kubernetes.io/component=api|api;$K8S_API|443|Kubernetes API;DENY"
  "shop|app.kubernetes.io/component=api|api;1.1.1.1|443|интернет (1.1.1.1);DENY"
)

printf '| %-24s | %-22s | %-5s | %-8s | %-8s | %s |\n' "Источник" "Назначение" "Порт" "Ожидание" "Факт" ""
printf '|%s|%s|%s|%s|%s|%s|\n' "--------------------------" "------------------------" "-------" "----------" "----------" "----"
fails=0
for row in "${ROWS[@]}"; do
  IFS=';' read -r SRC DST EXPECT <<<"$row"
  IFS='|' read -r SNS SSEL SNAME <<<"$SRC"
  IFS='|' read -r DIP DPORT DNAME <<<"$DST"
  GOT=$(verdict "$(check "$SNS" "$SSEL" "$DIP" "$DPORT")")
  if [ "$GOT" = "$EXPECT" ]; then MARK="✅"; else MARK="❌"; fails=$((fails+1)); fi
  printf '| %-24s | %-22s | %-5s | %-8s | %-8s | %s |\n' "$SNAME" "$DNAME" "$DPORT" "$EXPECT" "$GOT" "$MARK"
done
echo
[ "$fails" -eq 0 ] && echo "Матрица зелёная: все ${#ROWS[@]} проверок совпали с ожиданием." \
                  || echo "Расхождений: $fails из ${#ROWS[@]}."
