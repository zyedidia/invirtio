// SPDX-License-Identifier: GPL-2.0
/*
 * Privileged launcher for a UML driver guest.
 *
 * The launcher runs as root. It opens every host resource that the guest
 * needs, drops to an unprivileged user and executes UML with only those file
 * descriptors, so that UML opens no paths itself. It then supervises the
 * guest: it forwards SIGINT and SIGTERM, kills a guest that does not stop in
 * time, steers the devices' host interrupts and can restart the guest.
 */
#define _GNU_SOURCE
#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <limits.h>
#include <pwd.h>
#include <signal.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#include <linux/vfio.h>
#include <sys/ioctl.h>
#include <sys/personality.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>

#define MAX_DEVICES 8
#define MAX_ARGS 256

struct device {
	const char *addr;
	int group_id;
	int group_fd;
};

static const char *prog = "invirtio-launch";

static struct device devices[MAX_DEVICES];
static int nr_devices;
static bool bind_devices;
static const char *initrd_path;
static const char *user;
static uid_t uid;
static gid_t gid;
static unsigned long memlock_mib = 256;
static const char *cgroup_dir;
static const char *mconsole_path;
static const char *irq_cpus;
static int stop_timeout = 30;
static bool restart;
/* The UML binary, opened before privileges are dropped. */
static int uml_fd;

static void usage(void)
{
	fprintf(stderr,
"usage: %s [options] <uml binary> [uml arguments...]\n"
"\n"
"Run a UML driver guest as an unprivileged user. The launcher opens the\n"
"VFIO container and groups, the initramfs, /proc/cpuinfo and the mconsole\n"
"socket, and passes them to UML as file descriptors, along with the memfd\n"
"and no_uml_dir options. The guest's console is the launcher's stdin and\n"
"stdout.\n"
"\n"
"  -u <user>     run the guest as this user, given as a name or uid[:gid]\n"
"  -d <address>  pass through this PCI device, such as 0000:06:00.0. Can be\n"
"                repeated\n"
"  -b            bind the devices to vfio-pci if they are not bound to it\n"
"  -i <file>     the initramfs, passed as initrd=fd:<n>\n"
"  -l <MiB>      the guest's RLIMIT_MEMLOCK, which bounds pinned DMA memory\n"
"                (default 256)\n"
"  -g <dir>      run the guest in this existing cgroup\n"
"  -s <path>     serve the guest's mconsole on a socket at this path\n"
"  -a <cpus>     steer the devices' host interrupts to these host CPUs\n"
"  -t <seconds>  after asking the guest to stop, kill it if it has not\n"
"                exited within this time (default 30)\n"
"  -r            restart the guest when it exits with an error or a signal\n"
"\n"
"The first SIGINT or SIGTERM to the launcher is passed to the guest, which\n"
"then powers off in order. A second one kills the guest.\n",
		prog);
	exit(2);
}

static void die(const char *fmt, ...)
{
	va_list ap;

	fprintf(stderr, "%s: ", prog);
	va_start(ap, fmt);
	vfprintf(stderr, fmt, ap);
	va_end(ap);
	fputc('\n', stderr);
	exit(1);
}

static void warn(const char *fmt, ...)
{
	va_list ap;

	fprintf(stderr, "%s: ", prog);
	va_start(ap, fmt);
	vfprintf(stderr, fmt, ap);
	va_end(ap);
	fputc('\n', stderr);
}

/* Write a string to a sysfs or procfs file. */
static int write_file(const char *path, const char *s)
{
	int fd, n;

	fd = open(path, O_WRONLY | O_CLOEXEC);
	if (fd < 0)
		return -errno;
	n = write(fd, s, strlen(s));
	close(fd);
	if (n < 0)
		return -errno;
	return 0;
}

/* Read a short file into buf, without a trailing newline. */
static int read_file(const char *path, char *buf, size_t len)
{
	int fd, n;

	fd = open(path, O_RDONLY | O_CLOEXEC);
	if (fd < 0)
		return -errno;
	n = read(fd, buf, len - 1);
	close(fd);
	if (n < 0)
		return -errno;
	while (n > 0 && buf[n - 1] == '\n')
		n--;
	buf[n] = '\0';
	return 0;
}

/* Return the name of the device's driver in buf, or an empty string. */
static void device_driver(const char *addr, char *buf, size_t len)
{
	char path[PATH_MAX], link[PATH_MAX];
	ssize_t n;
	char *base;

	snprintf(path, sizeof(path), "/sys/bus/pci/devices/%s/driver", addr);
	n = readlink(path, link, sizeof(link) - 1);
	if (n < 0) {
		buf[0] = '\0';
		return;
	}
	link[n] = '\0';
	base = strrchr(link, '/');
	base = base ? base + 1 : link;
	n = strlen(base);
	if ((size_t)n >= len)
		n = len - 1;
	memcpy(buf, base, n);
	buf[n] = '\0';
}

static bool valid_address(const char *addr)
{
	static const char form[] = "xxxx:xx:xx.x";
	size_t i;

	if (strlen(addr) != strlen(form))
		return false;
	for (i = 0; form[i]; i++) {
		if (form[i] == 'x' ? !isxdigit((unsigned char)addr[i]) :
				     addr[i] != form[i])
			return false;
	}
	return addr[11] >= '0' && addr[11] <= '7';
}

/*
 * Bind the device to vfio-pci. driver_override is set first, so that the
 * device cannot go back to its own driver once it is unbound.
 */
static void bind_to_vfio(const char *addr, const char *driver)
{
	char path[PATH_MAX], now[64];
	int err;

	snprintf(path, sizeof(path), "/sys/bus/pci/devices/%s/driver_override",
		 addr);
	err = write_file(path, "vfio-pci");
	if (err)
		die("cannot set the driver override of %s: %s", addr,
		    strerror(-err));

	if (driver[0]) {
		snprintf(path, sizeof(path),
			 "/sys/bus/pci/devices/%s/driver/unbind", addr);
		err = write_file(path, addr);
		if (err)
			die("cannot unbind %s from %s: %s", addr, driver,
			    strerror(-err));
	}

	write_file("/sys/bus/pci/drivers_probe", addr);
	device_driver(addr, now, sizeof(now));
	if (strcmp(now, "vfio-pci"))
		die("binding %s to vfio-pci failed, its driver is '%s'", addr,
		    now);
	warn("bound %s to vfio-pci", addr);
}

/* Check the device's driver and find its IOMMU group. */
static void setup_device(struct device *dev)
{
	char path[PATH_MAX], link[PATH_MAX], driver[64];
	ssize_t n;
	char *base, *end;

	if (!valid_address(dev->addr))
		die("'%s' is not a PCI address like 0000:06:00.0", dev->addr);

	device_driver(dev->addr, driver, sizeof(driver));
	if (strcmp(driver, "vfio-pci")) {
		if (!bind_devices)
			die("%s is bound to '%s', not vfio-pci. Use -b to bind it",
			    dev->addr, driver);
		bind_to_vfio(dev->addr, driver);
	}

	snprintf(path, sizeof(path), "/sys/bus/pci/devices/%s/iommu_group",
		 dev->addr);
	n = readlink(path, link, sizeof(link) - 1);
	if (n < 0)
		die("%s has no IOMMU group: %s", dev->addr, strerror(errno));
	link[n] = '\0';
	base = strrchr(link, '/');
	base = base ? base + 1 : link;
	dev->group_id = strtol(base, &end, 10);
	if (*end || end == base)
		die("cannot parse the IOMMU group of %s", dev->addr);
}

/*
 * Parse a decimal id that ends at a colon or at the end of the string. Return
 * false if it is malformed or does not fit in 32 bits.
 */
static bool parse_id(const char *s, char **end, unsigned int *id)
{
	unsigned long v;

	if (!isdigit((unsigned char)*s))
		return false;
	errno = 0;
	v = strtoul(s, end, 10);
	if (errno || v > UINT_MAX || (**end && **end != ':'))
		return false;
	*id = v;
	return true;
}

static void parse_user(const char *s)
{
	struct passwd *pw;
	char *end;

	pw = getpwnam(s);
	if (pw) {
		uid = pw->pw_uid;
		gid = pw->pw_gid;
	} else {
		if (!parse_id(s, &end, &uid))
			die("unknown user '%s'", s);
		gid = uid;
		if (*end == ':' && (!parse_id(end + 1, &end, &gid) || *end))
			die("cannot parse the group in '%s'", s);
	}
	/* The set*id() calls treat an id of -1 as no change. */
	if (uid == 0 || uid == (uid_t)-1)
		die("the guest must not run as root");
	if (gid == 0 || gid == (gid_t)-1)
		die("the guest must not run in the root group");
}

/* The file descriptors that one run of the guest receives. */
struct guest_fds {
	int container;
	int initrd;
	int cpuinfo;
	int mconsole;
};

static int open_fd(const char *path, int flags)
{
	int fd = open(path, flags);

	if (fd < 0)
		die("cannot open %s: %s", path, strerror(errno));
	return fd;
}

/*
 * Bind the mconsole socket. A stale socket left by an earlier run is
 * removed first.
 */
static int bind_mconsole(const char *path)
{
	struct sockaddr_un addr = { .sun_family = AF_UNIX };
	int fd;

	if (strlen(path) >= sizeof(addr.sun_path))
		die("mconsole path '%s' is too long", path);
	strcpy(addr.sun_path, path);

	fd = socket(AF_UNIX, SOCK_DGRAM, 0);
	if (fd < 0)
		die("cannot create the mconsole socket: %s", strerror(errno));
	unlink(path);
	if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0)
		die("cannot bind %s: %s", path, strerror(errno));
	chmod(path, 0600);
	return fd;
}

/*
 * Open everything the guest needs. The descriptors are inherited across
 * exec, and the parent closes its copies once the guest has started.
 */
static void open_guest_fds(struct guest_fds *fds)
{
	struct vfio_group_status status = { .argsz = sizeof(status) };
	int i, j;

	fds->container = open_fd("/dev/vfio/vfio", O_RDWR);
	if (ioctl(fds->container, VFIO_GET_API_VERSION) != VFIO_API_VERSION)
		die("/dev/vfio/vfio has an unknown VFIO API version");

	for (i = 0; i < nr_devices; i++) {
		char path[64];

		devices[i].group_fd = -1;
		for (j = 0; j < i; j++) {
			if (devices[j].group_id == devices[i].group_id)
				devices[i].group_fd = devices[j].group_fd;
		}
		if (devices[i].group_fd >= 0)
			continue;

		snprintf(path, sizeof(path), "/dev/vfio/%d",
			 devices[i].group_id);
		devices[i].group_fd = open_fd(path, O_RDWR);
		if (ioctl(devices[i].group_fd, VFIO_GROUP_GET_STATUS,
			  &status) < 0)
			die("cannot get the status of %s: %s", path,
			    strerror(errno));
		if (!(status.flags & VFIO_GROUP_FLAGS_VIABLE))
			die("IOMMU group %d is not viable. Every device in it must be bound to vfio-pci",
			    devices[i].group_id);
	}

	fds->initrd = initrd_path ? open_fd(initrd_path, O_RDONLY) : -1;
	fds->cpuinfo = open_fd("/proc/cpuinfo", O_RDONLY);
	fds->mconsole = mconsole_path ? bind_mconsole(mconsole_path) : -1;
}

static void close_guest_fds(struct guest_fds *fds)
{
	int i, j;

	close(fds->container);
	for (i = 0; i < nr_devices; i++) {
		for (j = 0; j < i; j++) {
			if (devices[j].group_fd == devices[i].group_fd)
				break;
		}
		if (j == i)
			close(devices[i].group_fd);
	}
	if (fds->initrd >= 0)
		close(fds->initrd);
	close(fds->cpuinfo);
	if (fds->mconsole >= 0)
		close(fds->mconsole);
}

static char *format(const char *fmt, ...)
{
	va_list ap;
	char *s;

	va_start(ap, fmt);
	if (vasprintf(&s, fmt, ap) < 0)
		die("out of memory");
	va_end(ap);
	return s;
}

/*
 * Build UML's arguments. The ones for the passed fds come right after the
 * program name, so that a "--" from the caller cannot pass them to init.
 */
static char **guest_argv(int argc, char **argv, struct guest_fds *fds)
{
	static char *args[MAX_ARGS];
	int i, n = 0;

	if (argc + nr_devices + 8 > MAX_ARGS)
		die("too many arguments");

	args[n++] = argv[0];
	args[n++] = "memfd";
	args[n++] = "no_uml_dir";
	args[n++] = format("cpuinfo_fd=%d", fds->cpuinfo);
	if (fds->initrd >= 0)
		args[n++] = format("initrd=fd:%d", fds->initrd);
	if (fds->mconsole >= 0)
		args[n++] = format("mconsole=fd:%d", fds->mconsole);
	if (nr_devices)
		args[n++] = format("vfio_uml.container_fd=%d", fds->container);
	for (i = 0; i < nr_devices; i++)
		args[n++] = format("vfio_uml.device=%s,group_fd=%d",
				   devices[i].addr, devices[i].group_fd);
	for (i = 1; i < argc; i++)
		args[n++] = argv[i];
	args[n] = NULL;
	return args;
}

/*
 * Runs in the child. Drop every privilege and become UML. The environment is
 * empty, since UML needs none of it.
 */
static void exec_guest(char **args)
{
	struct rlimit lim = {
		.rlim_cur = memlock_mib << 20,
		.rlim_max = memlock_mib << 20,
	};
	char *envp[] = { NULL };
	char path[PATH_MAX];
	uid_t ruid, euid, suid;
	gid_t rgid, egid, sgid;
	sigset_t none;
	int err;

	if (cgroup_dir) {
		snprintf(path, sizeof(path), "%s/cgroup.procs", cgroup_dir);
		err = write_file(path, "0");
		if (err)
			die("cannot join cgroup %s: %s", cgroup_dir,
			    strerror(-err));
	}

	if (setrlimit(RLIMIT_MEMLOCK, &lim) < 0)
		die("cannot set RLIMIT_MEMLOCK: %s", strerror(errno));

	/* UML executes itself again unless this is already set. */
	if (personality(PER_LINUX | ADDR_NO_RANDOMIZE) < 0)
		die("cannot disable address space randomization: %s",
		    strerror(errno));

	if (chdir("/") < 0)
		die("cannot change to /: %s", strerror(errno));

	/*
	 * A new session has no controlling terminal, so the guest cannot inject
	 * input into the caller's terminal with TIOCSTI.
	 */
	if (setsid() < 0)
		die("cannot start a new session: %s", strerror(errno));

	if (setgroups(0, NULL) < 0 || setresgid(gid, gid, gid) < 0 ||
	    setresuid(uid, uid, uid) < 0)
		die("cannot switch to uid %u gid %u: %s", uid, gid,
		    strerror(errno));
	if (getresuid(&ruid, &euid, &suid) < 0 ||
	    getresgid(&rgid, &egid, &sgid) < 0 ||
	    ruid != uid || euid != uid || suid != uid ||
	    rgid != gid || egid != gid || sgid != gid)
		die("the switch to uid %u gid %u did not take effect", uid, gid);
	if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) < 0)
		die("cannot set no_new_privs: %s", strerror(errno));

	signal(SIGINT, SIG_DFL);
	signal(SIGTERM, SIG_DFL);
	signal(SIGCHLD, SIG_DFL);
	sigemptyset(&none);
	sigprocmask(SIG_SETMASK, &none, NULL);

	fexecve(uml_fd, args, envp);
	die("cannot execute %s: %s", args[0], strerror(errno));
}

/* The host CPU list as the kernel prints it, once it has been written. */
static char irq_target[256];

/*
 * Set the affinity of the devices' VFIO interrupts, which vfio-pci names
 * vfio-msi[N](<address>) or vfio-msix[N](<address>). They only exist while
 * the guest has MSI or MSI-X enabled, so this runs periodically.
 */
static void steer_irqs(void)
{
	char *line = NULL, path[64], cur[256], name[32];
	size_t size = 0;
	FILE *f;
	int i;

	f = fopen("/proc/interrupts", "re");
	if (!f)
		return;

	while (getline(&line, &size, f) > 0) {
		char *last, *colon;
		int irq;

		line[strcspn(line, "\n")] = '\0';
		last = strrchr(line, ' ');
		if (!last || strncmp(last + 1, "vfio-msi", 8))
			continue;
		for (i = 0; i < nr_devices; i++) {
			snprintf(name, sizeof(name), "(%s)", devices[i].addr);
			if (strstr(last, name))
				break;
		}
		if (i == nr_devices)
			continue;

		irq = strtol(line, &colon, 10);
		if (*colon != ':')
			continue;

		snprintf(path, sizeof(path), "/proc/irq/%d/smp_affinity_list",
			 irq);
		/* An interrupt can be freed at any time, so errors skip it. */
		if (read_file(path, cur, sizeof(cur)) ||
		    !strcmp(cur, irq_target))
			continue;
		if (write_file(path, irq_cpus)) {
			warn("cannot set the affinity of IRQ %d", irq);
			continue;
		}
		if (!read_file(path, cur, sizeof(cur)))
			snprintf(irq_target, sizeof(irq_target), "%s", cur);
		warn("IRQ %d -> CPU %s", irq, irq_target);
	}
	free(line);
	fclose(f);
}

static double now(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return ts.tv_sec + ts.tv_nsec / 1e9;
}

/* Set when the launcher has been asked to stop. */
static bool stopping;

/*
 * Wait for the guest to exit and return its wait status. SIGINT and SIGTERM
 * are blocked in the launcher and handled here.
 */
static int supervise(pid_t pid)
{
	struct timespec tick = { .tv_nsec = 200 * 1000 * 1000 };
	double deadline = 0;
	sigset_t set;
	int status, sig;

	sigemptyset(&set);
	sigaddset(&set, SIGINT);
	sigaddset(&set, SIGTERM);
	sigaddset(&set, SIGCHLD);

	for (;;) {
		if (waitpid(pid, &status, WNOHANG) == pid)
			return status;

		if (irq_cpus)
			steer_irqs();

		if (deadline && now() > deadline) {
			warn("the guest did not stop within %d seconds, killing it",
			     stop_timeout);
			kill(pid, SIGKILL);
			deadline = 0;
		}

		sig = sigtimedwait(&set, NULL, &tick);
		if (sig != SIGINT && sig != SIGTERM)
			continue;

		if (!stopping) {
			warn("stopping the guest");
			stopping = true;
			kill(pid, SIGTERM);
			deadline = now() + stop_timeout;
		} else {
			warn("killing the guest");
			kill(pid, SIGKILL);
		}
	}
}

/* The launcher's children, which main() checks that the kernel provides. */
static char children_path[64];

/*
 * Kill and reap every process that the guest left behind, so that none of
 * them keeps the devices. The launcher is a child subreaper, so they are all
 * its children.
 */
static void kill_orphans(void)
{
	struct timespec pause = { .tv_nsec = 10 * 1000 * 1000 };
	bool warned = false;
	pid_t pid;
	FILE *f;

	for (;;) {
		pid = waitpid(-1, NULL, WNOHANG);
		if (pid > 0)
			continue;
		if (pid < 0)
			return;

		f = fopen(children_path, "re");
		if (!f)
			die("cannot read %s: %s", children_path, strerror(errno));
		while (fscanf(f, "%d", &pid) == 1) {
			if (!warned) {
				warn("killing processes that the guest left behind");
				warned = true;
			}
			kill(pid, SIGKILL);
		}
		fclose(f);
		nanosleep(&pause, NULL);
	}
}

static int run_guest(int argc, char **argv)
{
	struct guest_fds fds;
	char **args;
	pid_t pid;
	int status;

	open_guest_fds(&fds);
	args = guest_argv(argc, argv, &fds);

	pid = fork();
	if (pid < 0)
		die("fork failed: %s", strerror(errno));
	if (pid == 0)
		exec_guest(args);

	/* The guest holds the only copies, so its exit releases the devices. */
	close_guest_fds(&fds);
	irq_target[0] = '\0';

	status = supervise(pid);
	kill_orphans();
	if (WIFEXITED(status)) {
		warn("the guest exited with status %d", WEXITSTATUS(status));
		return WEXITSTATUS(status);
	}
	warn("the guest was killed by signal %d", WTERMSIG(status));
	return 128 + WTERMSIG(status);
}

int main(int argc, char **argv)
{
	struct timespec pause = { .tv_sec = 1 };
	sigset_t set, stop;
	int opt, ret, i;
	char *end;

	/* Only the descriptors that the launcher opens reach the guest. */
	close_range(3, ~0U, 0);

	while ((opt = getopt(argc, argv, "+u:d:bi:l:g:s:a:t:rh")) != -1) {
		switch (opt) {
		case 'u':
			user = optarg;
			break;
		case 'd':
			if (nr_devices == MAX_DEVICES)
				die("at most %d devices are supported",
				    MAX_DEVICES);
			devices[nr_devices++].addr = optarg;
			break;
		case 'b':
			bind_devices = true;
			break;
		case 'i':
			initrd_path = optarg;
			break;
		case 'l':
			memlock_mib = strtoul(optarg, &end, 10);
			if (*end || end == optarg)
				die("cannot parse -l %s", optarg);
			break;
		case 'g':
			cgroup_dir = optarg;
			break;
		case 's':
			mconsole_path = optarg;
			break;
		case 'a':
			irq_cpus = optarg;
			break;
		case 't':
			stop_timeout = strtol(optarg, &end, 10);
			if (*end || end == optarg || stop_timeout < 0)
				die("cannot parse -t %s", optarg);
			break;
		case 'r':
			restart = true;
			break;
		default:
			usage();
		}
	}
	if (optind == argc || !user)
		usage();
	if (geteuid() != 0)
		die("must run as root");

	parse_user(user);
	for (i = 0; i < nr_devices; i++)
		setup_device(&devices[i]);
	uml_fd = open_fd(argv[optind], O_RDONLY | O_CLOEXEC);

	if (prctl(PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) < 0)
		die("cannot become a child subreaper: %s", strerror(errno));
	snprintf(children_path, sizeof(children_path),
		 "/proc/self/task/%d/children", getpid());
	if (access(children_path, R_OK) < 0)
		die("cannot read %s, which needs CONFIG_PROC_CHILDREN: %s",
		    children_path, strerror(errno));

	sigemptyset(&stop);
	sigaddset(&stop, SIGINT);
	sigaddset(&stop, SIGTERM);
	set = stop;
	sigaddset(&set, SIGCHLD);
	sigprocmask(SIG_BLOCK, &set, NULL);

	for (;;) {
		ret = run_guest(argc - optind, argv + optind);
		if (stopping || !restart || ret == 0)
			break;
		/* A stop request during the pause ends the launcher. */
		if (sigtimedwait(&stop, NULL, &pause) > 0)
			break;
		warn("restarting the guest");
	}

	if (mconsole_path)
		unlink(mconsole_path);
	return ret;
}
