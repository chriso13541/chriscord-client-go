#ifndef CHRISCORD_SYSAUDIO_H
#define CHRISCORD_SYSAUDIO_H
int sa_start(unsigned int pid, int exclude, char* err, int errlen);
int sa_read(float* out, int frames);
int sa_available(void);
void sa_stop(void);
#endif
