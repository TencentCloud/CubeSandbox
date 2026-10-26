#!/bin/bash
: '
    This script checks if an image is compatible with Cloud Hypervisor.
    At first, it detects the image type(raw or qcow2),
    partition type whether it is DOS or GPT.
    Then it mounts the image and checks if VIRTIO Configs
    are enabled in the kernel config. In the end, it provides
    a message about the compatibility of the image.
'



usage="$(basename "$0") [-h] -f -w -- program to check Cloud Hypervisor compatible image

where:
    -h  show this help text
    -f  image file location
    -w  directory to be used for temporary files"

function check_command {
    if ! command -v  $1 &> /dev/null
        then
            echo "Command $1 could not be found"
        exit 1
    fi
};

function check_if_root {
    if [ "$EUID" -ne 0 ]; then
        echo "Please run as root"
        exit 1
    fi

};

check_if_root
working_dir=""
while getopts ':hf:w:' option; do
    case "$option" in
        h) echo "$usage"
            exit
            ;;
        f) file_name=$OPTARG
            ;;
        w) working_dir=$OPTARG
            ;;
        :) printf "missing argument for -%s\n" "$OPTARG" >&2
            echo "$usage" >&2
            exit 1
            ;;
        \?) printf "illegal option: -%s\n" "$OPTARG" >&2
            echo "$usage" >&2
            exit 1
            ;;
    esac
done

shift $((OPTIND - 1))

if [ -z "${file_name}" ]; then
    echo "You must provide the image file name"
	exit 1
fi
if [[ ! -f ${file_name} ]]; then
    echo "File ${file_name} does not exist"
    exit 1
fi

file_abs_path=`readlink -m ${file_name}`
if [[ "${working_dir}" != "" && ! -d "${working_dir}" ]]; then
    echo "Directory ${working_dir} does not exist"
    exit 1
elif [[ "${working_dir}" == "" ]]; then
    working_dir=`mktemp -d`
    tmp_created=1
else
    working_dir=`readlink -m ${working_dir}`
fi

#get file extension and image type
extension="${file_name##*.}"
filename="${file_name%.*}"
dest_file=${working_dir}/${filename}.raw
image_type=$(qemu-img info ${file_abs_path} | grep 'file format:' | awk '{ print $3 }')
echo "Image type detected as ${image_type}"


if [[ "${image_type}" == "raw" ]]; then
	dest_file=${file_abs_path}
elif [[ "$image_type" == "qcow2" ]]; then
    if lsmod | grep "nbd" &> /dev/null ; then
        echo "Module nbd is loaded!"
    else
        echo "Module nbd is not loaded. Trying to load the module"
        modprobe nbd max_part=8
        if [ $? != 0 ]; then
            echo "failed to load nbd module. Exiting"
            exit 1
        fi
    fi
    check_command qemu-img
    dest_file=/dev/nbd0
    qemu-nbd --connect=${dest_file} ${file_abs_path} --read-only
fi

check_command blkid
#get part info
part_type=$(blkid -o value -s PTTYPE ${dest_file})

check_command partx
nr_partitions=`partx -g ${dest_file}  | wc -l`

check_command fdisk
out=`fdisk -l  ${dest_file} --bytes | grep -i -A ${nr_partitions} 'Device' | tail -n +2`

IFS='
'
i=0
declare -A liness
for x in $out ; do
	lines[$i]=$x
	i=$((i+1))
done

declare -A partitions
IFS=' '
i=0
ROWS=${#lines[@]}

for line in "${lines[@]}";
do
	j=0
	read -a str_arr <<< "$line"
	for val in "${str_arr[@]}";
	do
		if [[ "$val" != "*" ]]; then
			partitions[$i,$j]=$val
			j=$((j+1))
		fi
	done
	i=$((i+1))
done

COLUMNS=$j
COUNT=${#partitions[@]}
START_ADDRESS_INDEX=1
FILE_SYS_INDEX2=$((COLUMNS-1))
FILE_SYS_INDEX1=$((COLUMNS-2))
DEVICE_INDEX=0
# Here we have all the partition info now lets mount and analyze the contents
for ((i=0;i<ROWS;i++)) do
	if [[ "$part_type" == "gpt" && "${partitions[$i,${FILE_SYS_INDEX1}]}" == "Linux" && "${partitions[$i,${FILE_SYS_INDEX2}]}" == "filesystem" ]]; then
        echo "The image has GPT partitions"
        MOUNT_ROW=$i
		break
	elif [[ "$part_type" == "dos" && "${partitions[$i,${FILE_SYS_INDEX1}]}" == "Linux" && "${partitions[$i,${FILE_SYS_INDEX2}]}" == "" ]]; then
        echo "The image has DOS partitions"
        MOUNT_ROW=$i
        break
	fi
done

start_address=${partitions[${MOUNT_ROW},${START_ADDRESS_INDEX}]}
offset=$((start_address*512))

MOUNT_DIR=/mnt/clh-img-check/
rm -rf ${MOUNT_DIR}
mkdir ${MOUNT_DIR}
if [[ "${image_type}" == "raw" ]]; then
    mount -o ro,loop,offset=$offset ${dest_file} ${MOUNT_DIR}
elif [[ "${image_type}" == "qcow2" ]]; then
    mount -o ro ${partitions[${MOUNT_ROW},${DEVICE_INDEX}]} ${MOUNT_DIR}
fi

CONFIG_DIR=${MOUNT_DIR}boot/
if [[ "$part_type" == "dos" ]]; then
	CONFIG_DIR=${MOUNT_DIR}
fi

#check VIRTIO
HAS_VIRTIO=1
for conf_file in ${CONFIG_DIR}config*; do
    out=`grep -E "CONFIG_VIRTIO=y|CONFIG_VIRTIO_BLK=y|CONFIG_VIRTIO_BLK=m" ${conf_file} | wc -l`
    if [[ "$out" != "2" ]]; then
	    echo "VIRTIO not found"
		HAS_VIRTIO=0
	fi
done

#clean up
umount ${MOUNT_DIR}

if [[ "${tmp_created}" == "1" ]]; then
    rm -rf ${working_dir}
fi

if [[ "${image_type}" == "qcow2" ]];then
    qemu-nbd --disconnect ${dest_file} > /dev/null
fi

result=""
if [[ "${part_type}" == "dos" ]]; then
	result="dos mode not supported"
fi
if [[ "${HAS_VIRTIO}" == "0" ]]; then
	if [[ "$result" != "" ]]; then
		result="${result},"
	fi
	result="$result VirtIO module not found in the image"
fi
if [[ "$result" == "" ]];then
	echo "No incompatibilities found"
else
	echo "$result"
fi
