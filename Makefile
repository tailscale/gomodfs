usage:
	echo "see Makefile; for dev purposes only, not for building"

benchmark:
	@set -eu; \
	rm -rf /tmp/buildcache-gomodfs; \
	mkdir -p /tmp/buildcache-gomodfs; \
	export GOCACHE=/tmp/buildcache-gomodfs; \
	export GOMODCACHE="$${HOME}/mnt-gomodfs"; \
	cd "$${HOME}/src/tailscale.com"; \
	echo "full..."; \
	time go install tailscale.com/cmd/tailscaled; \
	echo "three incremental..."; \
	time go install tailscale.com/cmd/tailscaled; \
	time go install tailscale.com/cmd/tailscaled; \
	time go install tailscale.com/cmd/tailscaled
