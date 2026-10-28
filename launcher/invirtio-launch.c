// SPDX-License-Identifier: GPL-2.0
/*
 * Privileged launcher for a UML driver guest.
 *
 * The launcher runs as root. It opens every host resource that the guest
 * needs, drops to an unprivileged user and executes UML with only those file
 * descriptors, so that UML opens no paths itself. The guest runs in new
 * mount, network, IPC and PID namespaces with an empty root, under Landlock
 * and a seccomp filter. The launcher then supervises the guest: it asks the
 * guest to stop on SIGINT, SIGTERM or SIGHUP, kills a guest that does not
 * stop in time, relays the guest's console and its management console,
 * steers the devices' host interrupts and can restart the guest.
 */
#define _GNU_SOURCE
#include <ctype.h>
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <limits.h>
#include <poll.h>
#include <pthread.h>
#include <pwd.h>
#include <sched.h>
#include <signal.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#include <linux/audit.h>
#include <linux/filter.h>
#include <linux/landlock.h>
#include <linux/seccomp.h>
#include <linux/vfio.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/personality.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/signalfd.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/un.h>
#include <termios.h>
#include <sys/wait.h>

#define MAX_DEVICES 8
#define MAX_ARGS 256

struct device {
	const char *addr;
	/* The expansion ROM image to give the guest, or NULL. */
	const char *rom_path;
	int group_id;
	int group_fd;
	int rom_fd;
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
"VFIO container and groups, any ROM images, the initramfs and /proc/cpuinfo,\n"
"and passes them to UML as file descriptors, along with the memfd,\n"
"no_uml_dir, seccomp=on and sandbox options. The guest runs in new mount,\n"
"network, IPC and PID namespaces with an empty root, under Landlock and a\n"
"seccomp filter. When the launcher's stdin is a terminal, the guest gets a\n"
"pty of its own that the launcher relays to that terminal, so that the\n"
"guest does not hold it. Otherwise the guest inherits the launcher's stdin\n"
"and stdout.\n"
"\n"
"  -u <user>     run the guest as this user, given as a name or uid[:gid]\n"
"  -d <address>[,rom=<file>]\n"
"                pass through this PCI device, such as 0000:06:00.0. With\n"
"                rom, the guest sees the image in <file> as the device's\n"
"                expansion ROM. Can be repeated\n"
"  -b            bind the devices to vfio-pci if they are not bound to it\n"
"  -i <file>     the initramfs, passed as initrd=fd:<n>\n"
"  -l <MiB>      the guest's RLIMIT_MEMLOCK, which bounds the DMA memory that\n"
"                each of its processes pins (default 256)\n"
"  -g <dir>      run the guest in this existing cgroup. Its memory.max bounds\n"
"                all the memory the guest pins\n"
"  -s <path>     serve the guest's mconsole on a socket at this path, which\n"
"                the launcher relays to the guest\n"
"  -a <cpus>     steer the devices' host interrupts to these host CPUs\n"
"  -t <seconds>  after asking the guest to stop, kill it if it has not\n"
"                exited within this time (default 30)\n"
"  -r            restart the guest when it exits with an error or a signal\n"
"\n"
"A SIGINT, SIGTERM or SIGHUP to the launcher asks the guest to power off in\n"
"order, and a second one kills it, as does the launcher's own death. On a\n"
"terminal the guest's console is a pty, so Ctrl-C reaches the guest, not the\n"
"launcher, which is stopped with a signal instead.\n",
		prog);
	exit(2);
}

/*
 * Messages go through a pipe to a thread that writes them to stderr. The
 * guest shares the launcher's stderr and can keep a write to it waiting, on
 * a terminal behind the tty's write lock and on a file behind the inode's,
 * so the launcher and init write to the pipe, without blocking. A message
 * that does not fit in the pipe is dropped.
 */
static int msg_fd = 2;
static pthread_t msg_thread;
static pid_t launcher_pid;
/* Set while the guest's init is alive and holds a write end of the pipe. */
static bool guest_running;

/*
 * When the guest's console is a terminal, the launcher gives the guest a pty
 * of its own and relays it to the caller's terminal, so that the guest never
 * holds the caller's terminal and cannot change its line discipline, inject
 * input, or read its settings. pty_slave is the guest's end, on its fds 0 to
 * 2, and the launcher keeps pty_master.
 */
static int pty_master = -1;
static int pty_slave = -1;
/* The caller's terminal, for the guest's input and output, and for ioctls. */
static int term_in = -1;
static int term_out = -1;
static int term_ctl = -1;
/* Which of the guest's fds 0 to 2 are the caller's terminal. */
static unsigned int tty_fds;
static pthread_t console_thread;
static bool console_relaying;
static struct termios saved_termios;
static bool termios_raw;

static void vmessage(const char *fmt, va_list ap)
{
	char buf[512];
	int n;

	n = snprintf(buf, sizeof(buf) - 1, "%s: ", prog);
	n += vsnprintf(buf + n, sizeof(buf) - 1 - n, fmt, ap);
	if (n > (int)sizeof(buf) - 2)
		n = sizeof(buf) - 2;
	buf[n++] = '\n';
	n = write(msg_fd, buf, n);
}

/*
 * UML makes its console non-blocking, which stderr can share, so the thread
 * waits until stderr takes more.
 */
static void *write_messages(void *arg)
{
	struct pollfd out = { .fd = 2, .events = POLLOUT };
	int fd = (intptr_t)arg;
	char buf[4096];
	ssize_t n, off, w;

	while ((n = read(fd, buf, sizeof(buf))) > 0) {
		for (off = 0; off < n; off += w) {
			w = write(2, buf + off, n - off);
			if (w < 0 && errno != EAGAIN)
				break;
			if (w < 0) {
				poll(&out, 1, -1);
				w = 0;
			}
		}
	}
	return NULL;
}

/* Wait for the thread to write the messages that are left. */
static void flush_messages(void)
{
	if (msg_fd == 2)
		return;
	close(msg_fd);
	msg_fd = 2;
	pthread_join(msg_thread, NULL);
}

static void start_message_thread(void)
{
	sigset_t all, old;
	int pipefd[2], err;

	if (pipe2(pipefd, O_CLOEXEC) < 0)
		return;
	/* The main thread waits for the signals, so the new one takes none. */
	sigfillset(&all);
	pthread_sigmask(SIG_BLOCK, &all, &old);
	err = pthread_create(&msg_thread, NULL, write_messages,
			     (void *)(intptr_t)pipefd[0]);
	pthread_sigmask(SIG_SETMASK, &old, NULL);
	if (err || fcntl(pipefd[1], F_SETFL, O_NONBLOCK) < 0) {
		close(pipefd[1]);
		if (err)
			close(pipefd[0]);
		return;
	}
	msg_fd = pipefd[1];
}

static void flush_messages(void);
static void restore_terminal(void);

/*
 * The launcher waits for the message thread to write what is left before it
 * exits. While the guest's init holds a write end, the pipe would not end, so
 * the launcher then writes its last message to stderr itself.
 */
static void die(const char *fmt, ...)
{
	bool launcher = getpid() == launcher_pid;
	va_list ap;

	if (launcher && guest_running)
		msg_fd = 2;
	va_start(ap, fmt);
	vmessage(fmt, ap);
	va_end(ap);
	if (launcher) {
		restore_terminal();
		flush_messages();
	}
	exit(1);
}

static void warn(const char *fmt, ...)
{
	va_list ap;

	va_start(ap, fmt);
	vmessage(fmt, ap);
	va_end(ap);
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

/*
 * The file descriptors that one run of the guest receives, and the mconsole
 * sockets that the launcher keeps. The guest's mconsole socket is a
 * seqpacket pair, which ignores destination addresses, so that the guest
 * cannot send datagrams to other sockets through it.
 */
struct guest_fds {
	int container;
	int initrd;
	int cpuinfo;
	int mconsole_guest;
	int mconsole;
	int mconsole_relay;
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
	int fd, one = 1;
	mode_t mask;

	if (strlen(path) >= sizeof(addr.sun_path))
		die("mconsole path '%s' is too long", path);
	strcpy(addr.sun_path, path);

	fd = socket(AF_UNIX, SOCK_DGRAM | SOCK_CLOEXEC, 0);
	if (fd < 0)
		die("cannot create the mconsole socket: %s", strerror(errno));
	/* The relay measures how long each request waited. */
	if (setsockopt(fd, SOL_SOCKET, SO_TIMESTAMPNS, &one, sizeof(one)) < 0)
		die("cannot timestamp mconsole requests: %s", strerror(errno));
	unlink(path);
	mask = umask(077);
	if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0)
		die("cannot bind %s: %s", path, strerror(errno));
	umask(mask);
	return fd;
}

/*
 * Open everything the guest needs. The guest's descriptors are inherited
 * across exec, and the parent closes its copies once the guest has started.
 * The mconsole sockets that the parent keeps are close-on-exec.
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

		devices[i].rom_fd = devices[i].rom_path ?
			open_fd(devices[i].rom_path, O_RDONLY) : -1;

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
	fds->mconsole = -1;
	fds->mconsole_relay = -1;
	fds->mconsole_guest = -1;
	if (mconsole_path) {
		int pair[2];

		fds->mconsole = bind_mconsole(mconsole_path);
		if (socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0,
			       pair) < 0 ||
		    fcntl(pair[1], F_SETFD, 0) < 0)
			die("cannot create the guest's mconsole socket: %s",
			    strerror(errno));
		fds->mconsole_relay = pair[0];
		fds->mconsole_guest = pair[1];
	}
}

static void close_guest_fds(struct guest_fds *fds)
{
	int i, j;

	close(fds->container);
	for (i = 0; i < nr_devices; i++) {
		if (devices[i].rom_fd >= 0)
			close(devices[i].rom_fd);
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
	if (fds->mconsole_guest >= 0)
		close(fds->mconsole_guest);
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

	if (argc + nr_devices + 10 > MAX_ARGS)
		die("too many arguments");

	args[n++] = argv[0];
	args[n++] = "memfd";
	args[n++] = "no_uml_dir";
	args[n++] = "seccomp=on";
	args[n++] = "sandbox";
	args[n++] = format("cpuinfo_fd=%d", fds->cpuinfo);
	if (fds->initrd >= 0)
		args[n++] = format("initrd=fd:%d", fds->initrd);
	if (fds->mconsole_guest >= 0)
		args[n++] = format("mconsole=fd:%d", fds->mconsole_guest);
	if (nr_devices)
		args[n++] = format("vfio_uml.container_fd=%d", fds->container);
	for (i = 0; i < nr_devices; i++) {
		if (devices[i].rom_fd >= 0)
			args[n++] = format("vfio_uml.device=%s,group_fd=%d,rom_fd=%d",
					   devices[i].addr, devices[i].group_fd,
					   devices[i].rom_fd);
		else
			args[n++] = format("vfio_uml.device=%s,group_fd=%d",
					   devices[i].addr, devices[i].group_fd);
	}
	for (i = 1; i < argc; i++)
		args[n++] = argv[i];
	args[n] = NULL;
	return args;
}

static void drop_privileges(void)
{
	uid_t ruid, euid, suid;
	gid_t rgid, egid, sgid;

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
}

/*
 * Deny the guest every filesystem access except executing UML, and keep it
 * from signalling processes outside the sandbox and from connecting to
 * abstract Unix sockets. Landlock does not check files on internal mounts,
 * so the memfds that UML executes for guest processes are unaffected, and
 * files opened before the ruleset keep their access.
 */
static void restrict_filesystem(void)
{
	struct landlock_path_beneath_attr uml = {
		.allowed_access = LANDLOCK_ACCESS_FS_EXECUTE |
				  LANDLOCK_ACCESS_FS_READ_FILE,
		.parent_fd = uml_fd,
	};
	struct landlock_ruleset_attr attr = { 0 };
	int abi, fd;

	abi = syscall(SYS_landlock_create_ruleset, NULL, 0,
		      LANDLOCK_CREATE_RULESET_VERSION);
	if (abi < 0) {
		warn("Landlock is not available, the guest runs without it: %s",
		     strerror(errno));
		return;
	}

	attr.handled_access_fs = (LANDLOCK_ACCESS_FS_MAKE_SYM << 1) - 1;
	if (abi >= 2)
		attr.handled_access_fs |= LANDLOCK_ACCESS_FS_REFER;
	if (abi >= 3)
		attr.handled_access_fs |= LANDLOCK_ACCESS_FS_TRUNCATE;
	if (abi >= 5)
		attr.handled_access_fs |= LANDLOCK_ACCESS_FS_IOCTL_DEV;
	if (abi >= 4)
		attr.handled_access_net = LANDLOCK_ACCESS_NET_BIND_TCP |
					  LANDLOCK_ACCESS_NET_CONNECT_TCP;
	if (abi >= 6)
		attr.scoped = LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET |
			      LANDLOCK_SCOPE_SIGNAL;

	fd = syscall(SYS_landlock_create_ruleset, &attr, sizeof(attr), 0);
	if (fd < 0 ||
	    syscall(SYS_landlock_add_rule, fd, LANDLOCK_RULE_PATH_BENEATH,
		    &uml, 0) < 0 ||
	    syscall(SYS_landlock_restrict_self, fd, 0) < 0)
		die("cannot apply the Landlock ruleset: %s", strerror(errno));
	close(fd);
}

#define DENY(nr)							\
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, (nr), 0, 1),		\
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM)

#define CLONE_NAMESPACES (CLONE_NEWNS | CLONE_NEWCGROUP | CLONE_NEWUTS | \
			  CLONE_NEWIPC | CLONE_NEWUSER | CLONE_NEWPID | \
			  CLONE_NEWNET)

/*
 * System calls that UML never makes, mostly ones that reach large parts of
 * the host kernel, fail with EPERM. UML installs a tighter filter of its own
 * before its kernel starts, so this one covers UML's startup, and a UML
 * without the sandbox option. The guest's stub processes inherit it, and the
 * stub's own trap of every guest userspace system call must win, so this
 * filter fails calls instead of killing.
 */
static struct sock_filter launcher_filter[] = {
	BPF_STMT(BPF_LD | BPF_W | BPF_ABS,
		 offsetof(struct seccomp_data, arch)),
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, AUDIT_ARCH_X86_64, 1, 0),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM),
	BPF_STMT(BPF_LD | BPF_W | BPF_ABS,
		 offsetof(struct seccomp_data, nr)),
	BPF_JUMP(BPF_JMP | BPF_JSET | BPF_K, __X32_SYSCALL_BIT, 0, 1),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM),

	DENY(__NR_ptrace),
	DENY(__NR_process_vm_readv),
	DENY(__NR_process_vm_writev),
	DENY(__NR_mount),
	DENY(__NR_umount2),
	DENY(__NR_pivot_root),
	DENY(__NR_chroot),
	DENY(__NR_fsopen),
	DENY(__NR_fsconfig),
	DENY(__NR_fsmount),
	DENY(__NR_fspick),
	DENY(__NR_move_mount),
	DENY(__NR_open_tree),
	DENY(__NR_mount_setattr),
	DENY(__NR_unshare),
	DENY(__NR_setns),
	DENY(__NR_bpf),
	DENY(__NR_perf_event_open),
	DENY(__NR_userfaultfd),
	DENY(__NR_io_uring_setup),
	DENY(__NR_io_uring_enter),
	DENY(__NR_io_uring_register),
	DENY(__NR_add_key),
	DENY(__NR_request_key),
	DENY(__NR_keyctl),
	DENY(__NR_init_module),
	DENY(__NR_finit_module),
	DENY(__NR_delete_module),
	DENY(__NR_kexec_load),
	DENY(__NR_kexec_file_load),
	DENY(__NR_reboot),
	DENY(__NR_iopl),
	DENY(__NR_ioperm),
	DENY(__NR_swapon),
	DENY(__NR_swapoff),
	DENY(__NR_acct),
	DENY(__NR_quotactl),
	DENY(__NR_open_by_handle_at),
	DENY(__NR_name_to_handle_at),
	DENY(__NR_fanotify_init),
	DENY(__NR_syslog),

	/*
	 * Terminal ioctls that change the line discipline of the caller's
	 * terminal, which can load a module, or inject input into it.
	 */
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_ioctl, 0, 6),
	BPF_STMT(BPF_LD | BPF_W | BPF_ABS,
		 offsetof(struct seccomp_data, args[1])),
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, TIOCSETD, 3, 0),
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, TIOCSTI, 2, 0),
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, TIOCLINUX, 1, 0),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM),

	/* The flags of clone3 are in memory, so libc falls back to clone. */
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_clone3, 0, 1),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | ENOSYS),

	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_clone, 0, 4),
	BPF_STMT(BPF_LD | BPF_W | BPF_ABS,
		 offsetof(struct seccomp_data, args[0])),
	BPF_JUMP(BPF_JMP | BPF_JSET | BPF_K, CLONE_NAMESPACES, 1, 0),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM),

	/* Both create a socket of the family in their first argument. */
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_socket, 1, 0),
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_socketpair, 0, 4),
	BPF_STMT(BPF_LD | BPF_W | BPF_ABS,
		 offsetof(struct seccomp_data, args[0])),
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, AF_UNIX, 0, 1),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM),

	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
};

/*
 * Runs in the guest's PID namespace with the guest's credentials. Confine the
 * process and become UML. The environment is empty, since UML needs none of
 * it.
 */
static void exec_guest(char **args)
{
	struct sock_fprog prog = {
		.len = sizeof(launcher_filter) / sizeof(launcher_filter[0]),
		.filter = launcher_filter,
	};
	char *envp[] = { NULL };
	sigset_t none;

	/* UML executes itself again unless this is already set. */
	if (personality(PER_LINUX | ADDR_NO_RANDOMIZE) < 0)
		die("cannot disable address space randomization: %s",
		    strerror(errno));

	/*
	 * A new session has no controlling terminal, so the guest cannot inject
	 * input into the caller's terminal with TIOCSTI.
	 */
	if (setsid() < 0)
		die("cannot start a new session: %s", strerror(errno));

	restrict_filesystem();
	if (syscall(SYS_seccomp, SECCOMP_SET_MODE_FILTER, 0, &prog) < 0)
		die("cannot install the seccomp filter: %s", strerror(errno));

	signal(SIGINT, SIG_DFL);
	signal(SIGTERM, SIG_DFL);
	signal(SIGCHLD, SIG_DFL);
	sigemptyset(&none);
	sigprocmask(SIG_SETMASK, &none, NULL);

	fexecve(uml_fd, args, envp);
	die("cannot execute %s: %s. The root is empty, so UML must be linked statically",
	    args[0], strerror(errno));
}

/*
 * Give the guest an empty, read-only root, so that it can reach no host path.
 * A tmpfs mounted on /tmp becomes the root with pivot_root(".", "."), which
 * stacks the old root on top of it, and unmounting "." then drops the old
 * root.
 */
static void setup_root(void)
{
	if (mount(NULL, "/", NULL, MS_REC | MS_PRIVATE, NULL) < 0 ||
	    mount("tmpfs", "/tmp", "tmpfs", MS_NOSUID | MS_NODEV | MS_NOEXEC,
		  "mode=0555,size=4k") < 0 ||
	    chdir("/tmp") < 0 ||
	    syscall(SYS_pivot_root, ".", ".") < 0 ||
	    umount2(".", MNT_DETACH) < 0 ||
	    chdir("/") < 0 ||
	    mount(NULL, "/", NULL, MS_REMOUNT | MS_BIND | MS_RDONLY |
		  MS_NOSUID | MS_NODEV | MS_NOEXEC, NULL) < 0)
		die("cannot give the guest an empty root: %s", strerror(errno));
}

/*
 * Runs as the first process of the guest's PID namespace, which ends when it
 * exits. It finishes the namespaces, starts UML and passes SIGINT and SIGTERM
 * on to it. Apart from SIGKILL and SIGSTOP, a signal from outside the
 * namespace only reaches its first process if that process handles it or
 * blocks it, and run_guest() clones init with these signals blocked.
 */
static void guest_init(char **args, int alive[2])
{
	struct rlimit lim = {
		.rlim_cur = memlock_mib << 20,
		.rlim_max = memlock_mib << 20,
	};
	struct pollfd launcher = { .fd = alive[0] };
	char path[PATH_MAX];
	int status, sig, err;
	sigset_t set;
	pid_t pid, w;

	/* The launcher may have the same pid in its own namespace. */
	launcher_pid = 0;
	close(alive[1]);

	if (cgroup_dir) {
		snprintf(path, sizeof(path), "%s/cgroup.procs", cgroup_dir);
		err = write_file(path, "0");
		if (err)
			die("cannot join cgroup %s: %s", cgroup_dir,
			    strerror(-err));
	}

	/* A new session drops the caller's controlling terminal. */
	if (setsid() < 0)
		die("cannot start a new session: %s", strerror(errno));

	setup_root();

	if (setrlimit(RLIMIT_MEMLOCK, &lim) < 0)
		die("cannot set RLIMIT_MEMLOCK: %s", strerror(errno));

	/* UML gets the same credentials, so that init can always signal it. */
	drop_privileges();

	/*
	 * The guest must not outlive the launcher. Changing credentials clears
	 * the parent-death signal, so it is set here, and the pipe, whose write
	 * end only the launcher holds, shows whether the launcher died before.
	 */
	if (prctl(PR_SET_PDEATHSIG, SIGKILL, 0, 0, 0) < 0)
		die("cannot set the parent-death signal: %s", strerror(errno));
	if (poll(&launcher, 1, 0) != 0)
		_exit(1);

	/* The guest's console is its own pty, not the caller's terminal. */
	if (pty_slave >= 0) {
		if (tty_fds & 1)
			dup2(pty_slave, 0);
		if (tty_fds & 2)
			dup2(pty_slave, 1);
		if (tty_fds & 4)
			dup2(pty_slave, 2);
	}

	pid = fork();
	if (pid < 0)
		die("fork failed: %s", strerror(errno));
	if (pid == 0)
		exec_guest(args);

	if (msg_fd > 2)
		msg_fd = dup2(msg_fd, 3);
	close_range(msg_fd > 2 ? 4 : 3, ~0U, 0);

	sigemptyset(&set);
	sigaddset(&set, SIGINT);
	sigaddset(&set, SIGTERM);
	sigaddset(&set, SIGCHLD);
	for (;;) {
		sig = sigwaitinfo(&set, NULL);
		if (sig == SIGINT || sig == SIGTERM) {
			kill(pid, sig);
			continue;
		}
		while ((w = waitpid(-1, &status, WNOHANG)) > 0) {
			if (w != pid)
				continue;
			if (WIFSIGNALED(status)) {
				warn("the guest was killed by signal %d",
				     WTERMSIG(status));
				_exit(128 + WTERMSIG(status));
			}
			warn("the guest exited with status %d",
			     WEXITSTATUS(status));
			_exit(WEXITSTATUS(status));
		}
	}
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
 * The header of mconsole requests and replies. UML reads requests into a
 * static buffer, and it runs the command of a request even after it reports a
 * wrong version, so the relay passes on only complete version 2 requests,
 * each of which gets exactly one reply.
 */
#define MCONSOLE_MAGIC 0xcafebabe
#define MCONSOLE_VERSION 2
#define MCONSOLE_MAX_DATA 512
#define MCONSOLE_HEADER 12
#define MCONSOLE_REPLY_MORE 4

/*
 * Reply packets reach the client only within this many seconds of its
 * request's arrival, and later ones are dropped, as are requests that waited
 * that long, since the launcher resolves the client's address for each
 * packet. After this many packets in one reply, the relay stops reading the
 * guest's mconsole until the guest restarts.
 */
#define MCONSOLE_FORWARD_SECONDS 10
#define MCONSOLE_REPLY_PACKETS 65536

/*
 * The mconsole relay. It passes one request at a time to the guest and waits,
 * however long it takes, for the reply's last packet, since a later reply
 * cannot be told apart from it. It reads the guest's socket only while a
 * reply is due, so that a guest that sends packets unasked cannot keep the
 * launcher busy, and drops those packets before the next request.
 */
struct relay {
	int host;
	int guest;
	struct sockaddr_un client;
	socklen_t client_len;
	bool waiting;
	double forward_deadline;
	unsigned int packets;
};

static void relay_error(struct relay *r, const char *msg)
{
	struct {
		uint32_t err, more, len;
		char data[MCONSOLE_MAX_DATA];
	} reply = { .err = 1, .len = strlen(msg) + 1 };

	memcpy(reply.data, msg, reply.len);
	sendto(r->host, &reply, MCONSOLE_HEADER + reply.len, MSG_DONTWAIT,
	       (struct sockaddr *)&r->client, r->client_len);
}

static void relay_request(struct relay *r)
{
	char control[CMSG_SPACE(sizeof(struct timespec))];
	struct timespec sent = { 0 }, real;
	uint32_t header[3] = { 0 };
	struct cmsghdr *cmsg;
	struct msghdr msg;
	struct iovec iov;
	char buf[4096];
	double age;
	ssize_t n;
	int i;

	/* The guest can keep sending, so the number of packets is bounded. */
	for (i = 0; i < 64 && r->guest >= 0; i++) {
		if (recv(r->guest, buf, sizeof(buf), MSG_DONTWAIT) < 0)
			break;
	}

	iov = (struct iovec){ .iov_base = buf, .iov_len = sizeof(buf) };
	msg = (struct msghdr){
		.msg_name = &r->client,
		.msg_namelen = sizeof(r->client),
		.msg_iov = &iov,
		.msg_iovlen = 1,
		.msg_control = control,
		.msg_controllen = sizeof(control),
	};
	n = recvmsg(r->host, &msg, MSG_DONTWAIT);
	if (n < 0)
		return;
	r->client_len = msg.msg_namelen;

	for (cmsg = CMSG_FIRSTHDR(&msg); cmsg; cmsg = CMSG_NXTHDR(&msg, cmsg)) {
		if (cmsg->cmsg_level == SOL_SOCKET &&
		    cmsg->cmsg_type == SCM_TIMESTAMPNS)
			memcpy(&sent, CMSG_DATA(cmsg), sizeof(sent));
	}
	clock_gettime(CLOCK_REALTIME, &real);
	age = real.tv_sec - sent.tv_sec + (real.tv_nsec - sent.tv_nsec) / 1e9;
	/*
	 * A backward step of the clock makes the age too small, and negative
	 * if the step is larger than the age.
	 */
	if (age < 0 || age > MCONSOLE_FORWARD_SECONDS)
		return;

	if (n >= MCONSOLE_HEADER)
		memcpy(header, buf, sizeof(header));
	if (header[0] != MCONSOLE_MAGIC || header[1] != MCONSOLE_VERSION ||
	    header[2] >= MCONSOLE_MAX_DATA || MCONSOLE_HEADER + header[2] > n) {
		relay_error(r, "The launcher relays only version 2 requests");
		return;
	}
	if (r->guest < 0 || send(r->guest, buf, n, MSG_DONTWAIT) < 0) {
		relay_error(r, "The guest's mconsole is not available");
		return;
	}
	r->waiting = true;
	r->forward_deadline = now() + MCONSOLE_FORWARD_SECONDS - age;
	r->packets = 0;
}

static void relay_reply(struct relay *r, short revents)
{
	char buf[4096];
	uint32_t more = 0;
	ssize_t n;

	n = recv(r->guest, buf, sizeof(buf), MSG_DONTWAIT);
	if (n < 0)
		return;
	/* A zero-length packet is a reply, unless the guest hung up. */
	if (n == 0 && (revents & POLLHUP)) {
		r->guest = -1;
		r->waiting = false;
		return;
	}
	if (++r->packets > MCONSOLE_REPLY_PACKETS) {
		warn("the guest's mconsole reply has more than %d packets, the relay stops until the guest restarts",
		     MCONSOLE_REPLY_PACKETS);
		r->guest = -1;
		r->waiting = false;
		return;
	}
	if (now() <= r->forward_deadline)
		sendto(r->host, buf, n, MSG_DONTWAIT,
		       (struct sockaddr *)&r->client, r->client_len);
	if (n >= MCONSOLE_HEADER)
		memcpy(&more, buf + MCONSOLE_REPLY_MORE, sizeof(more));
	if (!more)
		r->waiting = false;
}

/*
 * Wait for the guest to exit and return its wait status. The stop signals
 * that the caller does not ignore are blocked in the launcher and handled
 * here, and mconsole requests are relayed to the guest.
 */
static int supervise(pid_t pid, struct guest_fds *fds)
{
	struct relay relay = {
		.host = fds->mconsole,
		.guest = fds->mconsole_relay,
	};
	struct signalfd_siginfo si;
	struct pollfd poll_fds[3];
	double deadline = 0, next_steer = 0;
	int status, sfd;
	sigset_t set;

	sigemptyset(&set);
	sigaddset(&set, SIGINT);
	sigaddset(&set, SIGTERM);
	sigaddset(&set, SIGHUP);
	sigaddset(&set, SIGWINCH);
	sigaddset(&set, SIGCHLD);
	sfd = signalfd(-1, &set, SFD_CLOEXEC);
	if (sfd < 0)
		die("cannot create a signalfd: %s", strerror(errno));

	for (;;) {
		if (waitpid(pid, &status, WNOHANG) == pid) {
			close(sfd);
			return status;
		}

		if (irq_cpus && now() >= next_steer) {
			steer_irqs();
			next_steer = now() + 0.2;
		}

		if (deadline && now() > deadline) {
			kill(pid, SIGKILL);
			deadline = 0;
			warn("the guest did not stop within %d seconds, killed it",
			     stop_timeout);
		}
		/* poll() skips entries with a negative fd. */
		poll_fds[0] = (struct pollfd){ .fd = sfd, .events = POLLIN };
		poll_fds[1] = (struct pollfd){
			.fd = relay.waiting ? relay.guest : -1,
			.events = POLLIN,
		};
		poll_fds[2] = (struct pollfd){
			.fd = relay.waiting ? -1 : relay.host,
			.events = POLLIN,
		};
		if (poll(poll_fds, 3, 200) <= 0)
			continue;

		if (poll_fds[1].revents & POLLIN) {
			relay_reply(&relay, poll_fds[1].revents);
		} else if (poll_fds[1].revents) {
			relay.guest = -1;
			relay.waiting = false;
		}
		if (poll_fds[2].revents)
			relay_request(&relay);

		if (!(poll_fds[0].revents & POLLIN) ||
		    read(sfd, &si, sizeof(si)) != sizeof(si) ||
		    si.ssi_signo == SIGCHLD)
			continue;

		if (si.ssi_signo == SIGWINCH) {
			struct winsize ws;

			if (pty_master >= 0 && term_ctl >= 0 &&
			    ioctl(term_ctl, TIOCGWINSZ, &ws) == 0)
				ioctl(pty_master, TIOCSWINSZ, &ws);
			continue;
		}

		if (!stopping) {
			kill(pid, SIGTERM);
			stopping = true;
			deadline = now() + stop_timeout;
			warn("stopping the guest");
		} else {
			kill(pid, SIGKILL);
			warn("killed the guest");
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

/*
 * Open a pty for the guest's console when the caller's console is a terminal.
 * The guest gets the slave on its fds 0 to 2, so its con0=fd:0,fd:1 uses it.
 */
static void open_console(void)
{
	struct termios t;
	struct winsize ws;
	char name[64];
	int i;

	pty_master = -1;
	pty_slave = -1;
	term_in = term_out = term_ctl = -1;
	tty_fds = 0;
	for (i = 0; i < 3; i++)
		if (isatty(i))
			tty_fds |= 1u << i;
	if (!tty_fds)
		return;

	/* Read the guest's input from a terminal stdin, and send its output to
	 * a terminal stdout, stderr or stdin, whichever comes first. */
	if (tty_fds & 1)
		term_in = 0;
	term_out = (tty_fds & 2) ? 1 : (tty_fds & 4) ? 2 : 0;
	term_ctl = (tty_fds & 1) ? 0 : (tty_fds & 2) ? 1 : 2;

	pty_master = posix_openpt(O_RDWR | O_NOCTTY | O_CLOEXEC);
	if (pty_master < 0 || grantpt(pty_master) < 0 ||
	    unlockpt(pty_master) < 0 ||
	    ptsname_r(pty_master, name, sizeof(name)) != 0)
		die("cannot create the guest's console: %s", strerror(errno));
	pty_slave = open(name, O_RDWR | O_NOCTTY | O_CLOEXEC);
	if (pty_slave < 0)
		die("cannot open the guest's console: %s", strerror(errno));

	/* Start the guest's pty from the caller's terminal settings. */
	if (tcgetattr(term_ctl, &t) == 0)
		tcsetattr(pty_slave, TCSANOW, &t);
	if (ioctl(term_ctl, TIOCGWINSZ, &ws) == 0)
		ioctl(pty_slave, TIOCSWINSZ, &ws);
}

/*
 * Put the caller's terminal in raw mode, so that the guest's pty does the line
 * discipline, and restore it when the guest is gone.
 */
static void enter_raw(void)
{
	struct termios raw;

	if (tcgetattr(term_ctl, &saved_termios) < 0)
		return;
	raw = saved_termios;
	cfmakeraw(&raw);
	if (tcsetattr(term_ctl, TCSANOW, &raw) == 0)
		termios_raw = true;
}

static void restore_terminal(void)
{
	if (termios_raw) {
		tcsetattr(term_ctl, TCSANOW, &saved_termios);
		termios_raw = false;
	}
}

/*
 * Copy between the caller's terminal and the guest's pty. It runs in a thread
 * of its own with blocking writes, so that a terminal that is slow to drain
 * delays no signal handling or guest supervision. It ends when the guest's
 * pty hangs up.
 */
static void *relay_console(void *arg)
{
	struct pollfd p[2];
	char buf[4096];
	ssize_t n, off, w;

	(void)arg;
	p[0] = (struct pollfd){ .fd = term_in, .events = POLLIN };
	p[1] = (struct pollfd){ .fd = pty_master, .events = POLLIN };
	for (;;) {
		if (poll(p, 2, -1) < 0) {
			if (errno == EINTR)
				continue;
			break;
		}
		if (p[0].fd >= 0 && (p[0].revents & POLLIN)) {
			n = read(p[0].fd, buf, sizeof(buf));
			if (n <= 0) {
				/* The input is gone, but output still flows. */
				p[0].fd = -1;
			} else {
				for (off = 0; off < n; off += w)
					if ((w = write(pty_master, buf + off,
							n - off)) <= 0)
						break;
			}
		}
		if (p[1].revents & (POLLIN | POLLHUP | POLLERR)) {
			n = read(pty_master, buf, sizeof(buf));
			if (n <= 0)
				break;
			if (term_out >= 0)
				for (off = 0; off < n; off += w)
					if ((w = write(term_out, buf + off,
						       n - off)) <= 0)
						break;
		}
	}
	return NULL;
}

/* Start the console relay with every signal blocked, so it takes none. */
static void start_console(void)
{
	sigset_t all, old;

	close(pty_slave);
	pty_slave = -1;
	enter_raw();
	sigfillset(&all);
	pthread_sigmask(SIG_BLOCK, &all, &old);
	console_relaying = pthread_create(&console_thread, NULL, relay_console,
					  NULL) == 0;
	pthread_sigmask(SIG_SETMASK, &old, NULL);
}

static void stop_console(void)
{
	if (console_relaying) {
		pthread_cancel(console_thread);
		pthread_join(console_thread, NULL);
		console_relaying = false;
	}
	if (pty_master >= 0) {
		close(pty_master);
		pty_master = -1;
	}
	restore_terminal();
}

static int run_guest(int argc, char **argv)
{
	struct guest_fds fds;
	int status, alive[2];
	sigset_t set, old;
	char **args;
	pid_t pid;

	open_guest_fds(&fds);
	args = guest_argv(argc, argv, &fds);
	open_console();
	if (pipe2(alive, O_CLOEXEC) < 0)
		die("cannot create a pipe: %s", strerror(errno));

	/*
	 * The child is the first process of a new PID namespace, and it reports
	 * how UML exited. A stop signal that the caller ignored is not blocked
	 * in the launcher, and the first process of a PID namespace drops an
	 * ignored signal that is not blocked, so init starts with its signals
	 * blocked.
	 */
	sigemptyset(&set);
	sigaddset(&set, SIGINT);
	sigaddset(&set, SIGTERM);
	sigaddset(&set, SIGCHLD);
	sigprocmask(SIG_BLOCK, &set, &old);
	pid = syscall(SYS_clone, CLONE_NEWNS | CLONE_NEWNET | CLONE_NEWIPC |
		      CLONE_NEWPID | SIGCHLD, NULL, NULL, NULL, NULL);
	if (pid == 0)
		guest_init(args, alive);
	sigprocmask(SIG_SETMASK, &old, NULL);
	if (pid < 0)
		die("cannot create the guest's namespaces: %s", strerror(errno));
	guest_running = true;
	close(alive[0]);
	if (pty_master >= 0)
		start_console();

	/* The guest holds the only copies, so its exit releases the devices. */
	close_guest_fds(&fds);
	irq_target[0] = '\0';

	status = supervise(pid, &fds);
	guest_running = false;
	stop_console();
	close(alive[1]);
	if (fds.mconsole >= 0) {
		close(fds.mconsole);
		close(fds.mconsole_relay);
	}
	kill_orphans();
	if (WIFEXITED(status))
		return WEXITSTATUS(status);
	warn("the guest was killed by signal %d", WTERMSIG(status));
	return 128 + WTERMSIG(status);
}

static const int stop_signals[] = { SIGINT, SIGTERM, SIGHUP };

int main(int argc, char **argv)
{
	struct timespec pause = { .tv_sec = 1 };
	struct sigaction old;
	sigset_t set, stop;
	int opt, ret, i;
	char *end;

	/* die() tells the launcher from its children even if no thread starts. */
	launcher_pid = getpid();

	/* Only the descriptors that the launcher opens reach the guest. */
	close_range(3, ~0U, 0);
	start_message_thread();

	while ((opt = getopt(argc, argv, "+u:d:bi:l:g:s:a:t:rh")) != -1) {
		switch (opt) {
		case 'u':
			user = optarg;
			break;
		case 'd':
			if (nr_devices == MAX_DEVICES)
				die("at most %d devices are supported",
				    MAX_DEVICES);
			devices[nr_devices].addr = optarg;
			end = strchr(optarg, ',');
			if (end) {
				*end++ = '\0';
				if (strncmp(end, "rom=", 4) || !end[4])
					die("cannot parse -d option '%s'", end);
				devices[nr_devices].rom_path = end + 4;
			}
			nr_devices++;
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

	/*
	 * An inherited SIG_IGN would make the kernel reap the guest without
	 * reporting it to waitpid().
	 */
	signal(SIGCHLD, SIG_DFL);
	/*
	 * The guest may set TOSTOP on the console, which would otherwise stop a
	 * launcher in the background when it writes a message.
	 */
	signal(SIGTTOU, SIG_IGN);
	/* A write to a console or pipe whose reader has gone must not kill us. */
	signal(SIGPIPE, SIG_IGN);

	/*
	 * A stop signal that the caller ignores stays ignored, such as SIGHUP
	 * under nohup or SIGINT in a background job.
	 */
	sigemptyset(&stop);
	for (i = 0; i < (int)(sizeof(stop_signals) / sizeof(stop_signals[0]));
	     i++) {
		if (sigaction(stop_signals[i], NULL, &old) < 0 ||
		    old.sa_handler != SIG_IGN)
			sigaddset(&stop, stop_signals[i]);
	}
	set = stop;
	sigaddset(&set, SIGCHLD);
	sigaddset(&set, SIGWINCH);
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
	flush_messages();
	return ret;
}
