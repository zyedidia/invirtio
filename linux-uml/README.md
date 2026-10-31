This is the Linux UML tree for InvirtIO. This holds the source code for the
Linux guest that runs drivers in userspace via UML. This tree contains
significant modifications to UML mmap'd BARs, lazy DMA mapping, faster IRQ
delivery, and VDUSE transport for several VirtIO device types. Upstream UML
also already includes support for SMP.
