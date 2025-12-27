#!/usr/bin/env bash
# ==============================================================================
# disk-setup.sh - Mithril Storage Setup
# ==============================================================================
#
# PURPOSE:
# Sets up NVMe storage optimally for Mithril. This script helps you configure
# your drives for the best performance during block verification.
#
# ==============================================================================
# UNDERSTANDING MITHRIL'S STORAGE NEEDS
# ==============================================================================
#
# Mithril stores three types of data, each with different I/O patterns:
#
# 1. ACCOUNTSDB (Most Critical - Needs Fastest Drive)
#    What: The state of all Solana accounts (balances, program data, etc.)
#    I/O pattern: Small RANDOM reads/writes - millions per second
#    Why critical: Slow random I/O = slow block verification
#    Size: ~500 GB currently (reserve 700 GB to be safe)
#
# 2. SNAPSHOTS (Can Use Slower Drive)
#    What: Downloaded Solana snapshots from the network
#    I/O pattern: Large SEQUENTIAL streaming reads during sync
#    Size: ~100 GB per snapshot
#
# 3. BLOCKSTORE (Optional - Can Use Slower Drive)
#    What: Verified blocks after processing
#    I/O pattern: SEQUENTIAL writes, parallel I/O during verification
#    Size: Configurable - depends on how many blocks you want to store
#
# OVER-PROVISIONING (IMPORTANT FOR SSD LONGEVITY AND PERFORMANCE):
#   Leave 15-20% of your SSD unallocated (no partition at all). This gives the
#   SSD controller guaranteed "scratch space" for garbage collection and wear
#   leveling. Performance degrades significantly when drives exceed 80% full.
#
#   Static OP (unallocated space) is better than dynamic OP (free space in a
#   partition that relies on TRIM commands). Static OP is always available to
#   the controller immediately.
#
#   For a 1 TB drive: leave ~150-200 GB unallocated.
#   For a 2 TB drive: leave ~300-400 GB unallocated.
#
# RECOMMENDED SETUP:
#   - Single Drive:  Put everything on your fastest NVMe (keep 20% unallocated)
#   - Two Drives:    AccountsDB on fast drive, snapshots/blockstore on second
#
# NOTE: Separate partitions on the SAME drive won't improve I/O performance -
#       the underlying storage is still shared. Partitions help with:
#       - Organization (e.g., reformat just AccountsDB without touching OS)
#       - Preventing disk-full issues in one area from affecting others
#       - Different mount options per partition
#       You need separate PHYSICAL drives for actual I/O isolation.
#
# ==============================================================================
# FILESYSTEM CHOICE: ext4 vs xfs
# ==============================================================================
#
# On modern NVMe, the difference is usually 0-10% for general workloads.
# Specific patterns can see 20-50% swings either way.
#
# ext4 tends to be better for:
#   + Small-file / metadata-heavy workloads (like AccountsDB random I/O)
#   + Lower latency variance on mixed workloads
#   + Safer default for general Linux use
#
# xfs tends to be better for:
#   + High parallelism / many threads doing I/O
#   + Large files + sustained throughput (streaming snapshots)
#   + Huge directories at scale
#   + Fast crash recovery (no long fsck times)
#
# RECOMMENDATION FOR MITHRIL:
#   - AccountsDB drive: ext4 (random I/O, small reads/writes)
#   - Snapshots/Blockstore drive: xfs may have edge (sequential, parallel I/O)
#   - Single drive setup: ext4 is the safer default
#
# We're still benchmarking - join #mithril-hardware on Discord for updates.
#
# ==============================================================================
# USAGE
# ==============================================================================
#
#   sudo ./scripts/disk-setup.sh --benchmark          # Test drive speeds
#   sudo ./scripts/disk-setup.sh --setup              # Format & configure
#   ./scripts/disk-setup.sh --status                  # Show current config
#   ./scripts/disk-setup.sh --disk-info               # Show UUIDs and device info
#
# DELETE OPTIONS (for resetting Mithril - start fresh):
#   --delete-accountsdb  Clear AccountsDB (forces new snapshot sync)
#   --delete-snapshots   Clear downloaded snapshots
#   --delete-blockstore  Clear verified blocks
#   --delete-all         Clear everything (complete reset)
#
# SAFETY:
#   - NEVER touches the root (boot) disk
#   - NEVER formats drives with mounted partitions
#   - Requires typing a confirmation phrase before any destructive action
#   - Creates backups of /etc/fstab before modification
#
# ==============================================================================

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

die()     { echo -e "\n${RED}[ERROR]${NC} $*\n" >&2; exit 1; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*" >&2; }
info()    { echo -e "\n${BLUE}[INFO]${NC} $*"; }
success() { echo -e "${GREEN}[OK]${NC} $*"; }

# Cleanup trap - ensures temp files are removed on exit/interrupt
cleanup() {
    rm -f /tmp/mithril_bench_* 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# Spinner for long-running operations
spinner() {
    local pid=$1
    local msg="${2:-Working}"
    local spin='⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏'
    local i=0
    while kill -0 "$pid" 2>/dev/null; do
        printf "\r  %s %s... " "${spin:i++%${#spin}:1}" "$msg"
        sleep 0.1
    done
    printf "\r  %-40s\n" ""
}

check_root() {
    [[ $EUID -eq 0 ]] || die "This script must be run as root. Try: sudo $0"
}

# Check and install required dependencies for disk operations
check_disk_deps() {
    local missing=()

    # Check for required commands
    command -v parted >/dev/null 2>&1 || missing+=("parted")
    command -v wipefs >/dev/null 2>&1 || missing+=("util-linux")  # wipefs is in util-linux
    command -v mkfs.ext4 >/dev/null 2>&1 || missing+=("e2fsprogs")
    command -v mkfs.xfs >/dev/null 2>&1 || missing+=("xfsprogs")

    if [[ ${#missing[@]} -gt 0 ]]; then
        echo ""
        warn "Missing required tools: ${missing[*]}"
        echo ""
        echo "These tools are needed for disk formatting operations."
        echo ""

        if yesno "Install missing dependencies now?"; then
            echo ""
            info "Installing: ${missing[*]}..."
            if apt-get update -qq && apt-get install -y -qq "${missing[@]}"; then
                info "Dependencies installed successfully."
            else
                die "Failed to install dependencies. Please install manually: sudo apt install ${missing[*]}"
            fi
        else
            die "Cannot proceed without required tools. Install with: sudo apt install ${missing[*]}"
        fi
    fi
}

yesno() {
    local prompt="$1" default="${2:-n}"
    local hint="[y/N]"
    [[ "${default,,}" == "y" ]] && hint="[Y/n]"
    local answer
    read -r -p "$prompt $hint: " answer
    answer="${answer:-$default}"
    [[ "${answer,,}" == "y" ]]
}

confirm_destructive() {
    local phrase="$1"
    echo ""
    echo -e "${RED}!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
    echo "DESTRUCTIVE ACTION - DATA WILL BE PERMANENTLY ERASED"
    echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
    echo -e "${NC}"
    echo "To proceed, type exactly:"
    echo -e "  ${YELLOW}$phrase${NC}"
    echo ""
    read -r -p "> " got
    [[ "$got" == "$phrase" ]] || die "Confirmation failed. Aborting."
}

# ------------------------------------------------------------------------------
# Disk Detection
# ------------------------------------------------------------------------------

get_root_disk() {
    local root_src root_pk
    root_src="$(findmnt -n -o SOURCE / 2>/dev/null || true)"

    # Handle rescue mode where root is overlay/tmpfs/loop
    if [[ -z "$root_src" ]] || [[ "$root_src" == "overlay" ]] || [[ "$root_src" == tmpfs* ]] || [[ "$root_src" == /dev/loop* ]]; then
        echo "none"  # No physical root disk (rescue/live mode)
        return 0
    fi

    root_pk="$(lsblk -no PKNAME "$root_src" 2>/dev/null || true)"
    if [[ -z "$root_pk" ]]; then
        echo "none"  # Can't determine parent disk
        return 0
    fi
    echo "/dev/$root_pk"
}

# SAFETY: Check if a path is on the root/OS disk
# Returns 0 (true) if the path is on the OS disk, 1 (false) otherwise
path_on_root_disk() {
    local path="$1"
    local root_disk
    root_disk=$(get_root_disk)

    # In rescue mode, there's no root disk to protect
    [[ "$root_disk" == "none" ]] && return 1

    # Get the device backing this path
    local path_device path_disk
    path_device="$(findmnt -n -o SOURCE --target "$path" 2>/dev/null || true)"

    # If path isn't mounted, it's not on root disk
    [[ -z "$path_device" ]] && return 1

    # Get the parent disk of this device
    path_disk="$(lsblk -no PKNAME "$path_device" 2>/dev/null || true)"
    [[ -z "$path_disk" ]] && return 1

    # Compare with root disk
    [[ "/dev/$path_disk" == "$root_disk" ]]
}

list_nvme_disks() {
    lsblk -dn -o NAME,TYPE | awk '$2=="disk" && $1~/^nvme/ {print "/dev/"$1}'
}

disk_has_mounts() {
    local disk="$1"
    lsblk -nr -o MOUNTPOINT "$disk" 2>/dev/null | grep -qv '^$'
}

disk_info() {
    local disk="$1"
    local size model
    size=$(lsblk -dn -o SIZE "$disk" 2>/dev/null | xargs)
    model=$(lsblk -dn -o MODEL "$disk" 2>/dev/null | xargs)
    echo "$size - $model"
}

disk_model() {
    local disk="$1"
    lsblk -dn -o MODEL "$disk" 2>/dev/null | xargs
}

disk_size() {
    local disk="$1"
    lsblk -dn -o SIZE "$disk" 2>/dev/null | xargs
}

part_path() {
    local disk="$1" partnum="$2"
    # NVMe uses 'p' prefix: /dev/nvme0n1p1
    local p="${disk}p${partnum}"
    [[ -b "$p" ]] && echo "$p" || echo "${disk}${partnum}"
}

# ------------------------------------------------------------------------------
# Benchmarking
# ------------------------------------------------------------------------------

# Check if fio is available
has_fio() {
    command -v fio >/dev/null 2>&1
}

# Check if hdparm is available
has_hdparm() {
    command -v hdparm >/dev/null 2>&1
}

# Random 4K read benchmark using fio on raw block device
# This is READ-ONLY and safe - does not write to the device
benchmark_fio_random_read() {
    local disk="$1"
    local runtime="${2:-15}"  # default 15 seconds

    if ! has_fio; then
        echo "0"
        return
    fi

    # Run fio directly on raw block device (read-only, no writes)
    local iops
    iops=$(fio --name=randread \
        --filename="$disk" \
        --direct=1 \
        --ioengine=libaio \
        --rw=randread \
        --bs=4k \
        --iodepth=32 \
        --numjobs=1 \
        --runtime="$runtime" \
        --time_based \
        --readonly \
        --group_reporting \
        --output-format=json 2>/dev/null | \
        grep -oP '"iops"\s*:\s*[\d.]+' | head -1 | grep -oP '[\d.]+' || echo "0")

    # Return IOPS (thousands)
    if [[ -n "$iops" && "$iops" != "0" ]]; then
        # Convert to K IOPS for cleaner display
        awk "BEGIN {printf \"%.1f\", $iops / 1000}"
    else
        echo "0"
    fi
}

# Sequential read benchmark using hdparm (fallback)
benchmark_sequential_read() {
    local disk="$1"

    if ! has_hdparm; then
        echo "0"
        return
    fi

    local speed
    speed=$(hdparm -t "$disk" 2>/dev/null | grep -oP '[\d.]+\s*MB/sec' | grep -oP '[\d.]+' | head -1 || echo "0")
    echo "${speed:-0}"
}

run_benchmarks() {
    info "Benchmarking NVMe drives..."
    echo ""
    echo "  This tests read performance to help determine which drive"
    echo "  should be used for AccountsDB (requires fastest random I/O)."
    echo ""

    local root_disk
    root_disk=$(get_root_disk)

    mapfile -t nvme_disks < <(list_nvme_disks)

    if [[ ${#nvme_disks[@]} -eq 0 ]]; then
        warn "No NVMe drives detected"
        return 1
    fi

    # Show detailed disk info
    echo "  ┌─────────────────────────────────────────────────────────────────────────┐"
    echo "  │ DETECTED NVME DRIVES                                                     │"
    echo "  ├─────────────────────────────────────────────────────────────────────────┤"
    for disk in "${nvme_disks[@]}"; do
        local model size note=""
        model=$(disk_model "$disk")
        size=$(disk_size "$disk")
        [[ "$disk" == "$root_disk" ]] && note=" (ROOT - will skip)"
        printf "  │ %-12s  %-8s  %-45s│\n" "$disk" "$size" "$model"
        if [[ -n "$note" ]]; then
            printf "  │              ${YELLOW}%s${NC}%-*s│\n" "$note" $((54 - ${#note})) ""
        fi
    done
    echo "  └─────────────────────────────────────────────────────────────────────────┘"
    echo ""

    # Determine benchmark mode based on available tools
    local use_fio=false
    local use_hdparm=false

    if has_fio; then
        use_fio=true
        echo "  Using fio for random 4K read benchmark (best for AccountsDB selection)"
        echo "  Test duration: ~15 seconds per drive"
    elif has_hdparm; then
        use_hdparm=true
        warn "fio not installed - using hdparm sequential read test (less accurate)"
        echo "  For better results, install fio: sudo apt install fio"
    else
        die "Neither fio nor hdparm installed. Install fio: sudo apt install fio"
    fi
    echo ""

    echo "  Running benchmarks..."
    echo ""

    declare -A results
    declare -A disk_models
    local metric_label="MB/s"
    local metric_desc="sequential"

    if $use_fio; then
        metric_label="K IOPS"
        metric_desc="random 4K"
    fi

    for disk in "${nvme_disks[@]}"; do
        [[ "$disk" == "$root_disk" ]] && continue

        local model
        model=$(disk_model "$disk")
        disk_models[$disk]="$model"

        # Run appropriate benchmark in background with spinner
        if $use_fio; then
            benchmark_fio_random_read "$disk" 15 > /tmp/mithril_bench_result_$$ 2>/dev/null &
        else
            benchmark_sequential_read "$disk" > /tmp/mithril_bench_result_$$ 2>/dev/null &
        fi

        local bench_pid=$!
        spinner $bench_pid "Testing $disk ($model)"
        wait $bench_pid 2>/dev/null || true

        local result
        result=$(cat /tmp/mithril_bench_result_$$ 2>/dev/null || echo "0")
        rm -f /tmp/mithril_bench_result_$$ 2>/dev/null || true

        results[$disk]="$result"

        success "  $disk: ${result} ${metric_label}"
    done

    echo ""
    echo "  ┌─────────────────────────────────────────────────────────────────────────┐"
    printf "  │ BENCHMARK RESULTS (%-13s read)                                   │\n" "$metric_desc"
    echo "  ├─────────────────────────────────────────────────────────────────────────┤"
    printf "  │ %-12s  %-12s  %-8s  %-32s│\n" "DEVICE" "SPEED" "SIZE" "MODEL"
    echo "  ├─────────────────────────────────────────────────────────────────────────┤"

    local best_disk="" best_speed=0
    for disk in "${!results[@]}"; do
        local speed="${results[$disk]}"
        local model="${disk_models[$disk]}"
        local size
        size=$(disk_size "$disk")

        # Truncate model if too long
        [[ ${#model} -gt 32 ]] && model="${model:0:29}..."

        printf "  │ %-12s  %6s %-5s  %-8s  %-32s│\n" "$disk" "$speed" "$metric_label" "$size" "$model"

        # Track best (use awk for float comparison - more portable than bc)
        if awk "BEGIN {exit !($speed > $best_speed)}" 2>/dev/null; then
            best_speed="$speed"
            best_disk="$disk"
        fi
    done

    echo "  └─────────────────────────────────────────────────────────────────────────┘"
    echo ""

    if [[ -n "$best_disk" ]]; then
        local best_model="${disk_models[$best_disk]}"
        success "Recommended for AccountsDB (fastest): $best_disk"
        echo "    Model: $best_model"
        echo ""
        if $use_fio; then
            echo "  Random 4K IOPS is the key metric for AccountsDB performance."
            echo "  Higher IOPS = faster block verification."
        else
            echo "  Note: Sequential read speed is a rough approximation."
            echo "  Install fio for accurate random I/O testing: sudo apt install fio"
        fi
        echo ""
        echo "  Use your fastest drive for AccountsDB, slower drive(s) for snapshots/blocks."
    fi
}

# ------------------------------------------------------------------------------
# Disk Info (for manual partitioning)
# ------------------------------------------------------------------------------

show_disk_info() {
    echo ""
    echo "================================================================================"
    echo "                    DISK INFORMATION FOR MANUAL PARTITIONING"
    echo "================================================================================"
    echo ""
    echo "Use this information when manually partitioning during Ubuntu installation"
    echo "or with tools like fdisk/parted/gparted."
    echo ""

    local root_disk
    root_disk=$(get_root_disk 2>/dev/null || echo "unknown")
    echo "Current root disk: $root_disk"
    echo ""

    echo "--- All Block Devices ---"
    echo ""
    lsblk -o NAME,SIZE,TYPE,MODEL,SERIAL,TRAN 2>/dev/null || lsblk -o NAME,SIZE,TYPE,MODEL
    echo ""

    echo "--- NVMe Drives (Detailed) ---"
    echo ""
    mapfile -t nvme_disks < <(list_nvme_disks)

    if [[ ${#nvme_disks[@]} -eq 0 ]]; then
        echo "  No NVMe drives detected"
    else
        for disk in "${nvme_disks[@]}"; do
            local model size serial
            model=$(lsblk -dn -o MODEL "$disk" 2>/dev/null | xargs)
            size=$(lsblk -dn -o SIZE "$disk" 2>/dev/null | xargs)
            serial=$(lsblk -dn -o SERIAL "$disk" 2>/dev/null | xargs)

            echo "  Device: $disk"
            echo "    Model:  $model"
            echo "    Size:   $size"
            [[ -n "$serial" ]] && echo "    Serial: $serial"

            # Show partitions and their UUIDs
            echo "    Partitions:"
            while IFS= read -r line; do
                local part_name part_size part_fstype part_uuid part_label
                part_name=$(echo "$line" | awk '{print $1}')
                part_size=$(echo "$line" | awk '{print $2}')
                part_fstype=$(echo "$line" | awk '{print $3}')
                part_uuid=$(echo "$line" | awk '{print $4}')
                part_label=$(echo "$line" | awk '{print $5}')

                if [[ "$part_name" != "$disk" && -n "$part_name" ]]; then
                    echo "      /dev/$part_name  ($part_size, $part_fstype)"
                    [[ -n "$part_uuid" && "$part_uuid" != "-" ]] && echo "        UUID:  $part_uuid"
                    [[ -n "$part_label" && "$part_label" != "-" ]] && echo "        Label: $part_label"
                fi
            done < <(lsblk -rno NAME,SIZE,FSTYPE,UUID,LABEL "$disk" 2>/dev/null)
            echo ""
        done
    fi

    echo "--- Partition UUIDs (for /etc/fstab) ---"
    echo ""
    echo "  Use these UUIDs in /etc/fstab for reliable mounting:"
    echo ""
    blkid | grep -E "nvme|sd" | while IFS= read -r line; do
        local dev uuid fstype label
        dev=$(echo "$line" | cut -d: -f1)
        uuid=$(echo "$line" | grep -oP 'UUID="[^"]+"' | head -1)
        fstype=$(echo "$line" | grep -oP 'TYPE="[^"]+"' | head -1)
        label=$(echo "$line" | grep -oP 'LABEL="[^"]+"' | head -1)

        echo "  $dev"
        [[ -n "$uuid" ]] && echo "    $uuid"
        [[ -n "$fstype" ]] && echo "    $fstype"
        [[ -n "$label" ]] && echo "    $label"
    done
    echo ""

    echo "--- Example fstab Entry ---"
    echo ""
    echo "  UUID=<your-uuid>  /mnt/mithril-accounts  ext4  defaults,noatime,nofail  0  2"
    echo ""
    echo "  The 'noatime' option reduces unnecessary writes (better for SSDs)."
    echo "  The 'nofail' option allows boot to continue if the drive is missing."
    echo ""

    echo "================================================================================"
}

# ------------------------------------------------------------------------------
# Status
# ------------------------------------------------------------------------------

show_status() {
    echo ""
    echo "================================================================================"
    echo "                    MITHRIL STORAGE STATUS"
    echo "================================================================================"
    echo ""

    local root_disk
    root_disk=$(get_root_disk)
    if [[ "$root_disk" == "none" ]]; then
        echo "Mode: Rescue/Live (no physical root disk)"
    else
        echo "Root disk: $root_disk (will never be modified)"
    fi
    echo ""

    echo "--- NVMe Drives ---"
    mapfile -t nvme_disks < <(list_nvme_disks)

    if [[ ${#nvme_disks[@]} -eq 0 ]]; then
        warn "No NVMe drives detected"
    else
        for disk in "${nvme_disks[@]}"; do
            local info note=""
            info=$(disk_info "$disk")
            [[ "$disk" == "$root_disk" ]] && note=" (ROOT)"
            echo "  $disk: $info$note"

            # Show partitions and mounts
            lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT "$disk" 2>/dev/null | tail -n +2 | sed 's/^/    /'
        done
    fi
    echo ""

    echo "--- Mithril Directories ---"
    local mithril_dirs=(
        "/mnt/mithril-accounts"
        "/mnt/mithril-ledger"
        "/mnt/mithril-ledger/snapshots"
        "/mnt/mithril-ledger/blockstore"
    )

    for dir in "${mithril_dirs[@]}"; do
        if [[ -d "$dir" ]]; then
            local mount_info
            mount_info=$(df -h "$dir" 2>/dev/null | tail -1 | awk '{print $1 " (" $4 " free)"}')
            success "$dir exists - $mount_info"
        else
            echo "  $dir - not found"
        fi
    done
    echo ""

    echo "--- Mithril fstab Entries ---"
    if grep -q mithril /etc/fstab 2>/dev/null; then
        grep mithril /etc/fstab | sed 's/^/  /'
    else
        echo "  No Mithril entries in fstab"
    fi

    echo ""
    echo "================================================================================"
}

# ------------------------------------------------------------------------------
# Setup
# ------------------------------------------------------------------------------

format_disk() {
    local disk="$1" fstype="$2" label="$3" overprovision="${4:-20}"

    info "Formatting $disk..."

    # Calculate partition size (leave space for over-provisioning)
    local use_percent=$((100 - overprovision))

    echo "  Over-provisioning: ${overprovision}% unallocated (${use_percent}% usable)"
    echo "  This improves SSD longevity and maintains consistent performance."

    # Wipe and create GPT
    wipefs -a "$disk"
    parted -s "$disk" mklabel gpt
    parted -s "$disk" mkpart primary 1MiB "${use_percent}%"

    local part
    part=$(part_path "$disk" 1)

    # Format
    case "$fstype" in
        ext4)
            mkfs.ext4 -F -L "$label" "$part"
            ;;
        xfs)
            mkfs.xfs -f -L "$label" "$part"
            ;;
        *)
            die "Unknown filesystem: $fstype"
            ;;
    esac

    echo "$part"
}

ask_filesystem() {
    local disk="$1"
    local purpose="${2:-general}"  # "accountsdb" or "data" or "general"
    local model
    model=$(disk_model "$disk")

    echo ""
    echo "  Filesystem for $disk ($model):"
    echo ""
    echo "    ext4: Mature, widely supported"
    echo "          + Better for small random I/O (like AccountsDB)"
    echo "          + Can shrink partitions offline"
    echo "          + Safer default choice"
    echo ""
    echo "    xfs:  High-performance for parallel/sequential I/O"
    echo "          + May be better for snapshots/blockstore"
    echo "          - Cannot shrink (must reformat to resize down)"
    echo ""

    if [[ "$purpose" == "accountsdb" ]]; then
        echo "  For AccountsDB (random I/O): ext4 recommended"
    elif [[ "$purpose" == "data" ]]; then
        echo "  For snapshots/blockstore (sequential I/O): xfs may have edge"
    fi
    echo ""

    local choice
    while true; do
        read -r -p "Enter filesystem (ext4/xfs) [ext4]: " choice
        choice="${choice:-ext4}"  # Default to ext4
        choice="${choice,,}"      # Lowercase

        case "$choice" in
            ext4|1) echo "ext4"; return ;;
            xfs|2)  echo "xfs"; return ;;
            *)
                echo "  Invalid choice '$choice'. Please enter 'ext4' or 'xfs'."
                ;;
        esac
    done
}

add_fstab_entry() {
    local part="$1" mountpoint="$2" fstype="$3"

    local uuid
    uuid=$(blkid -s UUID -o value "$part")

    # Backup fstab
    cp /etc/fstab "/etc/fstab.bak.$(date +%Y%m%d_%H%M%S)"

    # Check if entry already exists
    if grep -q "$uuid" /etc/fstab 2>/dev/null; then
        warn "UUID $uuid already in fstab, skipping"
        return
    fi

    # Add entry with mithril comment
    echo "# Mithril storage" >> /etc/fstab
    echo "UUID=$uuid  $mountpoint  $fstype  defaults,noatime,nofail  0  2" >> /etc/fstab

    success "Added fstab entry for $mountpoint"
}

interactive_setup() {
    info "MITHRIL DISK SETUP"
    echo ""
    echo "  This wizard helps you configure storage for Mithril."
    echo ""
    echo "  Mithril storage layout:"
    echo "    - AccountsDB: Requires FASTEST drive (random I/O during block replay)"
    echo "    - Snapshots:  Can use slower drive (sequential I/O)"
    echo "    - Blockstore: Can use slower drive (sequential I/O)"
    echo ""
    echo "  Recommended: Use separate drives for AccountsDB and snapshots/blockstore"
    echo "               if you have multiple NVMe drives."
    echo ""

    local root_disk
    root_disk=$(get_root_disk)

    if [[ "$root_disk" == "none" ]]; then
        echo "  Rescue/Live mode detected - no physical root disk to protect"
    else
        echo "  Root disk: $root_disk (will NEVER be touched)"
    fi
    echo ""

    # Find available disks
    mapfile -t nvme_disks < <(list_nvme_disks)
    local available_disks=()

    for disk in "${nvme_disks[@]}"; do
        [[ "$disk" == "$root_disk" ]] && continue
        available_disks+=("$disk")
    done

    if [[ ${#available_disks[@]} -eq 0 ]]; then
        die "No non-root NVMe drives available for setup"
    fi

    echo "  Available NVMe drives for Mithril:"
    for disk in "${available_disks[@]}"; do
        local info
        info=$(disk_info "$disk")
        local mounted=""
        disk_has_mounts "$disk" && mounted=" ${YELLOW}(HAS MOUNTED PARTITIONS)${NC}"
        echo -e "    $disk: $info$mounted"
    done
    echo ""

    if yesno "  Would you like to run benchmarks first to find the fastest drive?" "y"; then
        run_benchmarks
        echo ""
    fi

    # Configuration collection
    local accountsdb_disk="" accountsdb_mount="/mnt/mithril-accounts"
    local data_disk="" data_mount="/mnt/mithril-ledger"
    local accountsdb_fstype="" data_fstype=""

    echo ""
    echo "  STEP 1: AccountsDB Drive (most important - needs fastest drive)"
    echo ""

    if [[ ${#available_disks[@]} -eq 1 ]]; then
        echo "  Only one drive available: ${available_disks[0]}"
        echo "  AccountsDB, snapshots, and blockstore will share this drive."
        accountsdb_disk="${available_disks[0]}"

        if disk_has_mounts "$accountsdb_disk"; then
            die "Drive $accountsdb_disk has mounted partitions. Unmount first or use existing partitions."
        fi

        # Ask filesystem for the single drive (general purpose since it holds everything)
        accountsdb_fstype=$(ask_filesystem "$accountsdb_disk" "general")
    else
        echo "  Which drive should be used for AccountsDB?"
        echo "  (Choose your FASTEST drive based on benchmarks)"
        echo ""
        select disk in "${available_disks[@]}" "Skip (use existing)"; do
            if [[ "$disk" == "Skip (use existing)" ]]; then
                accountsdb_disk=""
                break
            elif [[ -n "$disk" ]]; then
                accountsdb_disk="$disk"
                break
            fi
        done

        if [[ -n "$accountsdb_disk" ]]; then
            if disk_has_mounts "$accountsdb_disk"; then
                die "Drive $accountsdb_disk has mounted partitions. Unmount first."
            fi
            # Ask filesystem for AccountsDB drive
            accountsdb_fstype=$(ask_filesystem "$accountsdb_disk" "accountsdb")
        fi

        echo ""
        echo "  STEP 2: Snapshots/Blockstore Drive"
        echo ""

        # Remove accountsdb disk from options
        local remaining_disks=()
        for disk in "${available_disks[@]}"; do
            [[ "$disk" != "$accountsdb_disk" ]] && remaining_disks+=("$disk")
        done

        if [[ ${#remaining_disks[@]} -gt 0 ]]; then
            echo "  Would you like to use a separate drive for snapshots/blockstore?"
            echo "  (This can be a slower drive)"
            echo ""
            select disk in "${remaining_disks[@]}" "Use same drive as AccountsDB" "Skip"; do
                case "$disk" in
                    "Use same drive as AccountsDB"|"Skip")
                        data_disk=""
                        ;;
                    *)
                        if [[ -n "$disk" ]]; then
                            data_disk="$disk"
                        fi
                        ;;
                esac
                break
            done

            if [[ -n "$data_disk" ]]; then
                if disk_has_mounts "$data_disk"; then
                    die "Drive $data_disk has mounted partitions. Unmount first."
                fi
                # Ask filesystem for data drive (snapshots/blockstore)
                data_fstype=$(ask_filesystem "$data_disk" "data")
            fi
        fi
    fi

    # Mount points
    echo ""
    echo "  STEP 3: Mount Points"
    echo ""

    read -r -p "  AccountsDB mount point [/mnt/mithril-accounts]: " input
    accountsdb_mount="${input:-/mnt/mithril-accounts}"

    if [[ -n "$data_disk" ]]; then
        read -r -p "  Ledger mount point (snapshots + blockstore) [/mnt/mithril-ledger]: " input
        data_mount="${input:-/mnt/mithril-ledger}"
    fi

    # Summary and confirmation
    echo ""
    echo "  ┌─────────────────────────────────────────────────────────────────────────┐"
    echo "  │ SETUP SUMMARY                                                            │"
    echo "  ├─────────────────────────────────────────────────────────────────────────┤"

    if [[ -n "$accountsdb_disk" ]]; then
        local adb_model
        adb_model=$(disk_model "$accountsdb_disk")
        printf "  │ AccountsDB:  %-58s │\n" "$accountsdb_disk -> $accountsdb_mount"
        printf "  │              %-58s │\n" "Model: $adb_model"
        printf "  │              %-58s │\n" "Filesystem: $accountsdb_fstype"
        echo -e "  │              ${RED}THIS DRIVE WILL BE ERASED${NC}                                 │"
    else
        echo "  │ AccountsDB:  (skipped - using existing)                                │"
    fi

    if [[ -n "$data_disk" ]]; then
        local data_model
        data_model=$(disk_model "$data_disk")
        printf "  │ Ledger:      %-58s │\n" "$data_disk -> $data_mount"
        printf "  │              %-58s │\n" "Model: $data_model"
        printf "  │              %-58s │\n" "Filesystem: $data_fstype"
        echo -e "  │              ${RED}THIS DRIVE WILL BE ERASED${NC}                                 │"
    elif [[ -n "$accountsdb_disk" ]]; then
        echo "  │ Data:        (same drive as AccountsDB)                                │"
    fi

    echo "  │                                                                          │"
    echo "  │ Directory structure to be created:                                       │"
    printf "  │   %-68s │\n" "$accountsdb_mount/accountsdb"
    if [[ -n "$data_disk" ]]; then
        printf "  │   %-68s │\n" "$data_mount/snapshots"
        printf "  │   %-68s │\n" "$data_mount/blockstore"
    else
        printf "  │   %-68s │\n" "$accountsdb_mount/snapshots"
        printf "  │   %-68s │\n" "$accountsdb_mount/blockstore"
    fi
    echo "  └─────────────────────────────────────────────────────────────────────────┘"
    echo ""

    if [[ -z "$accountsdb_disk" && -z "$data_disk" ]]; then
        info "No drives selected for formatting. Creating directories only."

        # Warn if mountpoint isn't actually a mounted disk
        if ! findmnt "$accountsdb_mount" >/dev/null 2>&1; then
            echo ""
            warn "WARNING: $accountsdb_mount is NOT a mounted disk."
            echo "  Directories will be created on your OS disk (likely /)"
            echo "  This may fill up your root filesystem with Mithril data."
            echo ""
            if ! yesno "  Continue anyway?" "n"; then
                echo "  Aborting. Mount a disk to $accountsdb_mount first."
                exit 1
            fi
        fi

        mkdir -p "$accountsdb_mount/accountsdb"
        mkdir -p "$accountsdb_mount/snapshots"
        mkdir -p "$accountsdb_mount/blockstore"

        success "Directories created"
        return
    fi

    # Final confirmation
    local disks_to_erase=""
    [[ -n "$accountsdb_disk" ]] && disks_to_erase="$accountsdb_disk"
    [[ -n "$data_disk" ]] && disks_to_erase="$disks_to_erase $data_disk"

    confirm_destructive "ERASE${disks_to_erase}"

    # Execute setup
    info "Starting disk setup..."

    # Format AccountsDB disk
    if [[ -n "$accountsdb_disk" ]]; then
        local accountsdb_part
        accountsdb_part=$(format_disk "$accountsdb_disk" "$accountsdb_fstype" "accountsdb")

        mkdir -p "$accountsdb_mount"
        mount "$accountsdb_part" "$accountsdb_mount"
        add_fstab_entry "$accountsdb_part" "$accountsdb_mount" "$accountsdb_fstype"

        # Create subdirectories
        mkdir -p "$accountsdb_mount/accountsdb"
        if [[ -z "$data_disk" ]]; then
            mkdir -p "$accountsdb_mount/snapshots"
            mkdir -p "$accountsdb_mount/blockstore"
        fi

        success "AccountsDB drive configured: $accountsdb_disk -> $accountsdb_mount ($accountsdb_fstype)"
    fi

    # Format data disk
    if [[ -n "$data_disk" ]]; then
        local data_part
        data_part=$(format_disk "$data_disk" "$data_fstype" "blockstore")

        mkdir -p "$data_mount"
        mount "$data_part" "$data_mount"
        add_fstab_entry "$data_part" "$data_mount" "$data_fstype"

        mkdir -p "$data_mount/snapshots"
        mkdir -p "$data_mount/blockstore"

        success "Data drive configured: $data_disk -> $data_mount ($data_fstype)"
    fi

    echo ""
    echo "  ┌─────────────────────────────────────────────────────────────────────────┐"
    echo "  │ SETUP COMPLETE                                                           │"
    echo "  ├─────────────────────────────────────────────────────────────────────────┤"
    echo "  │                                                                          │"
    echo "  │ Update your mithril.toml:                                                │"
    echo "  │                                                                          │"
    printf "  │   scratch_directory = \"%s\"%-*s│\n" "$accountsdb_mount" $((38 - ${#accountsdb_mount})) ""
    echo "  │                                                                          │"
    if [[ -n "$data_disk" ]]; then
        echo "  │   [snapshot]                                                             │"
        printf "  │       download_path = \"%s/snapshots\"%-*s│\n" "$data_mount" $((28 - ${#data_mount})) ""
    fi
    echo "  │                                                                          │"
    echo "  │ Next: Run performance-tune.sh to optimize I/O settings                   │"
    echo "  │                                                                          │"
    echo "  └─────────────────────────────────────────────────────────────────────────┘"
}

# ------------------------------------------------------------------------------
# Clean/Reset
# ------------------------------------------------------------------------------

find_mithril_dirs() {
    # Find directories that look like Mithril data dirs
    local dirs=()

    # Check common locations
    local common_paths=(
        "/mnt/mithril-accounts"
        "/mnt/mithril-ledger"
    )

    for path in "${common_paths[@]}"; do
        [[ -d "$path" ]] && dirs+=("$path")
    done

    # Also check any mount with "mithril" in the label
    while IFS= read -r mp; do
        [[ -d "$mp" ]] && [[ ! " ${dirs[*]} " =~ " ${mp} " ]] && dirs+=("$mp")
    done < <(findmnt -n -o TARGET | grep -i mithril 2>/dev/null || true)

    printf '%s\n' "${dirs[@]}"
}

dir_size() {
    local dir="$1"
    if [[ -d "$dir" ]]; then
        du -sh "$dir" 2>/dev/null | awk '{print $1}'
    else
        echo "0"
    fi
}

# Generic clean function for a specific subdirectory type
clean_subdir() {
    local subdir_name="$1"  # e.g., "accountsdb", "snapshots", "blockstore"
    local description="$2"  # e.g., "AccountsDB", "Snapshots", "Blockstore"

    info "DELETING ${description^^}"
    echo ""

    # Find Mithril directories
    mapfile -t mithril_dirs < <(find_mithril_dirs)

    if [[ ${#mithril_dirs[@]} -eq 0 ]]; then
        warn "No Mithril data directories found"
        echo ""
        echo "  Checked: /mnt/mithril-accounts, /mnt/mithril-ledger"
        return 1
    fi

    # Find all matching subdirectories
    local paths_to_clean=()

    echo "  Found $description directories:"
    echo ""

    for mithril_dir in "${mithril_dirs[@]}"; do
        if [[ -d "$mithril_dir/$subdir_name" ]]; then
            # SAFETY: Never delete anything on the OS disk
            if path_on_root_disk "$mithril_dir/$subdir_name"; then
                warn "SKIPPING $mithril_dir/$subdir_name - on OS disk (safety protection)"
                continue
            fi
            local size
            size=$(dir_size "$mithril_dir/$subdir_name")
            paths_to_clean+=("$mithril_dir/$subdir_name")
            echo "    $mithril_dir/$subdir_name  ($size)"
        fi
    done

    if [[ ${#paths_to_clean[@]} -eq 0 ]]; then
        warn "No $description directories found"
        return 1
    fi

    echo ""
    echo -e "  ${RED}These directories will be PERMANENTLY DELETED:${NC}"
    for path in "${paths_to_clean[@]}"; do
        echo "    $path"
    done
    echo ""

    confirm_destructive "DELETE $description"

    # Execute deletion
    info "Deleting $description..."

    for path in "${paths_to_clean[@]}"; do
        echo "  Removing $path..."
        rm -rf "$path"
        # Recreate empty directory
        mkdir -p "$path"
        success "Deleted: $path"
    done

    echo ""
    success "$description has been deleted"
}

clean_accountsdb() {
    clean_subdir "accountsdb" "AccountsDB"
    echo ""
    echo "  On next run, Mithril will rebuild AccountsDB from a fresh snapshot."
}

clean_snapshots() {
    clean_subdir "snapshots" "Snapshots"
    echo ""
    echo "  On next run, Mithril will download fresh snapshots from the network."
}

clean_blockstore() {
    clean_subdir "blockstore" "Blockstore"
    echo ""
    echo "  On next run, Mithril will rebuild blockstore from verified blocks."
}

clean_all() {
    info "DELETING ALL MITHRIL DATA"
    echo ""
    echo "  This will delete AccountsDB, Snapshots, AND Blockstore."
    echo ""

    # Find Mithril directories
    mapfile -t mithril_dirs < <(find_mithril_dirs)

    if [[ ${#mithril_dirs[@]} -eq 0 ]]; then
        warn "No Mithril data directories found"
        return 1
    fi

    # Find all subdirectories
    local paths_to_clean=()

    echo "  Found Mithril directories:"
    echo ""

    for mithril_dir in "${mithril_dirs[@]}"; do
        # SAFETY: Never delete anything on the OS disk
        if path_on_root_disk "$mithril_dir"; then
            warn "SKIPPING $mithril_dir - on OS disk (safety protection)"
            continue
        fi
        echo "    $mithril_dir"
        for subdir in accountsdb snapshots blockstore; do
            if [[ -d "$mithril_dir/$subdir" ]]; then
                local size
                size=$(dir_size "$mithril_dir/$subdir")
                paths_to_clean+=("$mithril_dir/$subdir")
                echo "      $subdir/  ($size)"
            fi
        done
    done

    if [[ ${#paths_to_clean[@]} -eq 0 ]]; then
        warn "No Mithril data directories found"
        return 1
    fi

    echo ""
    echo -e "  ${RED}ALL of the above directories will be PERMANENTLY DELETED${NC}"
    echo ""

    confirm_destructive "DELETE ALL MITHRIL DATA"

    # Execute deletion
    info "Deleting all Mithril data..."

    for path in "${paths_to_clean[@]}"; do
        echo "  Removing $path..."
        rm -rf "$path"
        mkdir -p "$path"
        success "Deleted: $path"
    done

    echo ""
    success "All Mithril data has been deleted"
    echo ""
    echo "  On next run, Mithril will start completely fresh."
}

# ------------------------------------------------------------------------------
# Main
# ------------------------------------------------------------------------------

show_help() {
    head -90 "$0" | tail -n +2 | grep -E "^#" | sed 's/^# \?//'
    exit 0
}

main() {
    local mode="${1:-}"

    case "$mode" in
        --benchmark)
            check_root
            run_benchmarks
            ;;
        --setup)
            check_root
            check_disk_deps
            interactive_setup
            ;;
        --delete-accountsdb)
            check_root
            clean_accountsdb
            ;;
        --delete-snapshots)
            check_root
            clean_snapshots
            ;;
        --delete-blockstore)
            check_root
            clean_blockstore
            ;;
        --delete-all)
            check_root
            clean_all
            ;;
        --status)
            show_status
            ;;
        --disk-info)
            show_disk_info
            ;;
        --help|-h)
            show_help
            ;;
        "")
            echo ""
            echo "Mithril Disk Setup"
            echo ""
            echo "Usage:"
            echo "  sudo ./scripts/disk-setup.sh --benchmark          # Test NVMe speeds (safe)"
            echo "  sudo ./scripts/disk-setup.sh --setup              # Interactive setup (formats drives)"
            echo "  ./scripts/disk-setup.sh --status                  # Show current storage status"
            echo "  ./scripts/disk-setup.sh --disk-info               # Show UUIDs and device info"
            echo ""
            echo "Delete commands (for resetting Mithril):"
            echo "  sudo ./scripts/disk-setup.sh --delete-accountsdb  # Delete AccountsDB only"
            echo "  sudo ./scripts/disk-setup.sh --delete-snapshots   # Delete snapshots only"
            echo "  sudo ./scripts/disk-setup.sh --delete-blockstore  # Delete blockstore only"
            echo "  sudo ./scripts/disk-setup.sh --delete-all         # Delete everything"
            echo ""
            echo "  ./scripts/disk-setup.sh --help                    # Show detailed help"
            echo ""
            exit 2
            ;;
        *)
            die "Unknown option: $mode. Use --help for usage."
            ;;
    esac
}

main "$@"
