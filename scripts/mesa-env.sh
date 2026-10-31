# Source this file from bash or zsh to run programs with the Mesa that
# scripts/mesa-build.sh built, for example
#   . scripts/mesa-env.sh && vulkaninfo --summary
# MESA_PREFIX selects another prefix than build/mesa.

MESA_PREFIX=${MESA_PREFIX:-$(realpath -m "$(dirname "${BASH_SOURCE[0]:-${(%):-%x}}")/../build/mesa")}
if [ ! -d "$MESA_PREFIX/lib" ]; then
	echo "mesa-env.sh: $MESA_PREFIX has no Mesa, run scripts/mesa-build.sh" >&2
	return 1
fi
export LD_LIBRARY_PATH=$MESA_PREFIX/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}
export VK_DRIVER_FILES=$MESA_PREFIX/share/vulkan/icd.d/radeon_icd.x86_64.json
export __EGL_VENDOR_LIBRARY_FILENAMES=$MESA_PREFIX/share/glvnd/egl_vendor.d/50_mesa.json
export GBM_BACKENDS_PATH=$MESA_PREFIX/lib/gbm
