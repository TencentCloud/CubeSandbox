/* Dedicated initramfs workload for the RAW discard POC. No production disks. */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/fs.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

static const char *mode;
static double now_ms(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return t.tv_sec * 1000.0 + t.tv_nsec / 1000000.0;
}
static void fail(const char *what) {
    printf("POC_JSON {\"stage\":\"fatal\",\"operation\":\"%s\",\"errno\":%d}\n",what,errno);
    fflush(stdout);
    for (;;) pause();
}
static void must(int ok, const char *what) { if (!ok) fail(what); }
static void directory(const char *p) { must(mkdir(p,0755)==0 || errno==EEXIST,p); }
static void checkpoint(const char *stage) {
    printf("POC_JSON {\"stage\":\"%s\",\"mode\":\"%s\"}\n",stage,mode);
    fflush(stdout);
    char c;
    must(read(0,&c,1)==1,"host-ack");
}
static double write_file(const char *p, size_t len, unsigned char byte) {
    unsigned char *buf=malloc(1024*1024);
    must(buf!=NULL,"malloc");
    memset(buf,byte,1024*1024);
    double t=now_ms();
    int fd=open(p,O_CREAT|O_TRUNC|O_WRONLY,0600);
    must(fd>=0,"open-write");
    while (len) {
        size_t want=len>1024*1024?1024*1024:len, done=0;
        while (done<want) {
            ssize_t n=write(fd,buf+done,want-done);
            if (n<0 && errno==EINTR) continue;
            must(n>0,"write");
            done+=n;
        }
        len-=want;
    }
    must(fsync(fd)==0,"fsync-file");
    must(close(fd)==0,"close-file");
    free(buf);
    return now_ms()-t;
}
static uint64_t hash_file(const char *p) {
    unsigned char *b=NULL;
    must(posix_memalign((void**)&b,4096,8192)==0,"hash-buffer");
    uint64_t h=14695981039346656037ULL;
    int fd=open(p,O_RDONLY|O_DIRECT);
    must(fd>=0,"open-direct-hash");
    ssize_t n;
    while ((n=read(fd,b,8192))>0)
        for(ssize_t i=0;i<n;i++) { h^=b[i]; h*=1099511628211ULL; }
    must(n==0,"read-hash");
    close(fd);
    free(b);
    return h;
}
static void trim(void) {
    int fd=open("/disk",O_RDONLY|O_DIRECTORY);
    must(fd>=0,"open-trim");
    struct fstrim_range range={.start=0,.len=UINT64_MAX,.minlen=0};
    double t=now_ms();
    must(ioctl(fd,FITRIM,&range)==0,"FITRIM");
    printf("POC_JSON {\"stage\":\"trim_result\",\"ms\":%.6f,\"requested_bytes\":%llu}\n",
           now_ms()-t,(unsigned long long)range.len);
    close(fd);
}
static int cmp_double(const void *a,const void *b) {
    double x=*(const double*)a, y=*(const double*)b;
    return (x>y)-(x<y);
}
int main(void) {
    setvbuf(stdout,NULL,_IOLBF,0);
    directory("/proc");directory("/sys");directory("/dev");
    must(mount("proc","/proc","proc",0,NULL)==0,"mount-proc");
    must(mount("sysfs","/sys","sysfs",0,NULL)==0,"mount-sys");
    must(mount("devtmpfs","/dev","devtmpfs",0,NULL)==0,"mount-dev");
    char cmdline[4096]={0};
    int fd=open("/proc/cmdline",O_RDONLY);must(fd>=0,"cmdline");
    must(read(fd,cmdline,sizeof(cmdline)-1)>0,"read-cmdline");close(fd);
    mode=strstr(cmdline,"poc.mode=online")?"online":strstr(cmdline,"poc.mode=batch")?"batch":"off";
    for(int i=0;i<200 && access("/dev/vda",F_OK)!=0;i++) usleep(50000);
    directory("/disk");directory("/lower");directory("/merged");
    must(mount("/dev/vda","/disk","ext4",0,!strcmp(mode,"online")?"barrier=0,discard":"barrier=0,nodiscard")==0,"mount-ext4");
    must(mount("tmpfs","/lower","tmpfs",0,"size=4m")==0,"mount-lower");
    directory("/disk/upper");directory("/disk/work");
    must(mount("overlay","/merged","overlay",0,"lowerdir=/lower,upperdir=/disk/upper,workdir=/disk/work")==0,"mount-overlay");
    char discard[80]={0};fd=open("/sys/block/vda/queue/discard_max_bytes",O_RDONLY);
    must(fd>=0,"discard-max");must(read(fd,discard,sizeof(discard)-1)>0,"read-discard");close(fd);
    printf("POC_JSON {\"stage\":\"device\",\"mode\":\"%s\",\"discard_max_bytes\":%llu}\n",mode,strtoull(discard,NULL,10));
    int disk=open("/disk",O_RDONLY|O_DIRECTORY);must(disk>=0,"disk-fd");
    write_file("/merged/keep",1024*1024,0x5a);
    uint64_t keep=hash_file("/merged/keep");
    must(syncfs(disk)==0,"sync-baseline");
    checkpoint("baseline");
    double write_ms=write_file("/merged/large",64*1024*1024,0xa5);
    must(syncfs(disk)==0,"sync-large");
    printf("POC_JSON {\"stage\":\"large_write_result\",\"ms\":%.6f,\"bytes\":67108864}\n",write_ms);
    checkpoint("large_written");
    double t=now_ms();
    must(unlink("/merged/large")==0,"delete-large");
    double unlink_ms=now_ms()-t, sync_start=now_ms();
    must(syncfs(disk)==0,"sync-delete");
    printf("POC_JSON {\"stage\":\"large_delete_result\",\"unlink_ms\":%.6f,\"sync_ms\":%.6f,\"total_ms\":%.6f}\n",
           unlink_ms,now_ms()-sync_start,now_ms()-t);
    checkpoint("large_deleted");
    if(!strcmp(mode,"batch")) { trim(); checkpoint("large_trimmed"); }
    directory("/merged/small");
    char name[160];
    unsigned char buf[65536]; memset(buf,0x69,sizeof buf);
    t=now_ms();
    for(int i=0;i<1000;i++) {
        snprintf(name,sizeof name,"/merged/small/f-%04d",i);
        fd=open(name,O_WRONLY|O_CREAT|O_EXCL,0600);must(fd>=0,"small-open");
        size_t pos=0;while(pos<sizeof buf) { ssize_t n=write(fd,buf+pos,sizeof buf-pos);must(n>0,"small-write");pos+=n; }
        must(close(fd)==0,"small-close");
    }
    must(syncfs(disk)==0,"small-write-sync");
    printf("POC_JSON {\"stage\":\"small_write_result\",\"ms\":%.6f,\"files\":1000,\"bytes\":65536000}\n",now_ms()-t);
    checkpoint("small_written");
    double latencies[1000];t=now_ms();
    for(int i=0;i<1000;i++) {
        snprintf(name,sizeof name,"/merged/small/f-%04d",i);
        double start=now_ms();must(unlink(name)==0,"small-unlink");latencies[i]=now_ms()-start;
    }
    unlink_ms=now_ms()-t;sync_start=now_ms();
    must(syncfs(disk)==0,"small-delete-sync");
    double sync_ms=now_ms()-sync_start;
    qsort(latencies,1000,sizeof(double),cmp_double);
    printf("POC_JSON {\"stage\":\"small_delete_result\",\"files\":1000,\"unlink_ms\":%.6f,\"sync_ms\":%.6f,\"total_ms\":%.6f,\"p50_ms\":%.6f,\"p95_ms\":%.6f,\"p99_ms\":%.6f,\"max_ms\":%.6f}\n",
           unlink_ms,sync_ms,unlink_ms+sync_ms,latencies[499],latencies[949],latencies[989],latencies[999]);
    checkpoint("small_deleted");
    if(!strcmp(mode,"batch")) { trim();checkpoint("small_trimmed"); }
    write_ms=write_file("/merged/reuse",32*1024*1024,0x3c);
    uint64_t reuse=hash_file("/merged/reuse");
    must(syncfs(disk)==0,"sync-reuse");
    printf("POC_JSON {\"stage\":\"reuse_write_result\",\"ms\":%.6f,\"keep_hash\":\"%016llx\",\"reuse_hash\":\"%016llx\"}\n",
           write_ms,(unsigned long long)keep,(unsigned long long)reuse);
    checkpoint("before_pause");
    must(hash_file("/merged/keep")==keep && hash_file("/merged/reuse")==reuse,"hash-after-resume");
    must(unlink("/merged/reuse")==0,"reuse-delete");
    must(syncfs(disk)==0,"reuse-delete-sync");
    if(!strcmp(mode,"batch"))trim();
    checkpoint("after_pause");
    must(hash_file("/merged/keep")==keep,"final-hash");
    printf("POC_JSON {\"stage\":\"done\",\"mode\":\"%s\",\"keep_hash\":\"%016llx\"}\n",mode,(unsigned long long)keep);
    fflush(stdout);sync();reboot(RB_POWER_OFF);
    for(;;)pause();
}
