#ifdef __wasi__
#include <sys/types.h>
extern "C" {
pid_t getppid(void);
mode_t umask(mode_t);
int chown(const char *, uid_t, gid_t);
int lchown(const char *, uid_t, gid_t);
}
#endif
