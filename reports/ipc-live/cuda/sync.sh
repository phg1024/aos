#!/bin/zsh
# Puts what a run needs on the machine. Usage: AOS_IPC_HOST=<ssh name> sync.sh. Builds the Linux binary here;
# the CPU reference is not in git. Its Mac binary stays behind, and the verifier rebuilds the baseline.
set -e
: ${AOS_IPC_HOST:?}
root=${0:A:h:h:h:h}
cd $root
GOOS=linux GOARCH=amd64 go build -buildvcs=false -o /tmp/aos-linux-$$ ./cmd/aos
ssh $AOS_IPC_HOST 'mkdir -p aos-ipc-live/bin aos-ipc-live/workflows aos-ipc-live/reports/rasterizer-bunny'
rsync -a /tmp/aos-linux-$$ "${AOS_IPC_HOST}:aos-ipc-live/bin/aos"
rm -f /tmp/aos-linux-$$
rsync -a reports/ipc-live/render.py reports/ipc-live/analyze.py reports/ipc-live/cuda/job.sh reports/ipc-live/cuda/test_steps.sh reports/ipc-live/cuda/test_path.sh reports/ipc-live/preflight/fake_model.py reports/ipc-live/preflight/metal.py "${AOS_IPC_HOST}:aos-ipc-live/bin/"
rsync -a workflows/rasterizer-bunny-cuda-handles-ipc.json "${AOS_IPC_HOST}:aos-ipc-live/workflows/"
rsync -a skills "${AOS_IPC_HOST}:aos-ipc-live/"
rsync -a --exclude rasterizer --exclude '*.ppm' reports/rasterizer-bunny/baseline reports/rasterizer-bunny/assets reports/rasterizer-bunny/scene-closeup.json reports/rasterizer-bunny/scene-side.json reports/rasterizer-bunny/scene-small.json reports/rasterizer-bunny/baseline_benchmark.json reports/rasterizer-bunny/optimized_benchmark.json reports/rasterizer-bunny/final-summary.txt "${AOS_IPC_HOST}:aos-ipc-live/reports/rasterizer-bunny/"
echo synced
