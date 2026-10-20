// seccomp-run — минимальный загрузчик seccomp-фильтра для Лабы 1.
//
//   seccomp-run <syscall>[,<syscall>...] -- <program> [args...]
//
// Разрешает все системные вызовы, кроме перечисленных: на них ядро вернёт EPERM.
// Фильтр ставится в последний момент перед execve(), как это делает runc,
// и наследуется всеми потомками программы.
//
// Сборка (в ВМ): sudo apt install -y gcc libseccomp-dev
//                gcc -O2 -o ~/bin/seccomp-run seccomp-run.c -lseccomp
#include <errno.h>
#include <seccomp.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int main(int argc, char **argv) {
	if (argc < 4 || strcmp(argv[2], "--") != 0) {
		fprintf(stderr, "usage: %s <syscall>[,<syscall>...] -- <program> [args...]\n", argv[0]);
		return 2;
	}

	// SCMP_ACT_ALLOW по умолчанию; libseccomp сам выставит no_new_privs перед загрузкой.
	scmp_filter_ctx ctx = seccomp_init(SCMP_ACT_ALLOW);
	if (!ctx) {
		fprintf(stderr, "seccomp_init failed\n");
		return 1;
	}

	char *list = strdup(argv[1]);
	for (char *name = strtok(list, ","); name; name = strtok(NULL, ",")) {
		int nr = seccomp_syscall_resolve_name(name);
		if (nr == __NR_SCMP_ERROR) {
			fprintf(stderr, "unknown syscall: %s\n", name);
			return 1;
		}
		if (nr < 0) {
			// Псевдо-номер: такого вызова нет на этой архитектуре (например, mkdir на arm64).
			fprintf(stderr, "warning: syscall %s does not exist on this arch, skipped\n", name);
			continue;
		}
		int rc = seccomp_rule_add(ctx, SCMP_ACT_ERRNO(EPERM), nr, 0);
		if (rc < 0) {
			fprintf(stderr, "seccomp_rule_add(%s): %s\n", name, strerror(-rc));
			return 1;
		}
		fprintf(stderr, "seccomp: deny %s (nr %d)\n", name, nr);
	}

	int rc = seccomp_load(ctx);
	if (rc < 0) {
		fprintf(stderr, "seccomp_load: %s\n", strerror(-rc));
		return 1;
	}
	seccomp_release(ctx);

	execvp(argv[3], &argv[3]);
	perror("execvp");
	return 127;
}
