SUBDIRS := vduse virtio_wlan

modules:
	@for d in $(SUBDIRS); do $(MAKE) -C $$d modules || exit 1; done

clean:
	@for d in $(SUBDIRS); do $(MAKE) -C $$d clean; done
