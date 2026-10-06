// System audio capture for screen sharing on Windows: WASAPI "process
// loopback" (Windows 10 2004 and later). Exclude mode records everything
// playing on the computer except this app and the processes it started (so
// a call's voices aren't sent back into the call); include mode records
// only one program and what it started (the window being shared).
//
// Process loopback is only reachable through ActivateAudioInterfaceAsync
// on the virtual device "VAD\Process_Loopback" — it can't be opened as an
// ordinary audio device. Everything below runs on its own thread: COM set
// up for that thread, the activation, then a loop pulling captured audio
// into a small ring buffer that Go reads (sa_read) every 10 ms.

#define COBJMACROS
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <objbase.h>
#include <audioclient.h>
#include <mmdeviceapi.h>
#include <stdio.h>
#include <string.h>

#include "sysaudio_windows.h"

// Declared here rather than taken from audioclientactivationparams.h,
// which older MinGW headers don't have.
typedef struct {
    DWORD TargetProcessId;
    int   ProcessLoopbackMode; // 0 include target process tree, 1 exclude it
} cc_process_loopback_params;
typedef struct {
    int ActivationType; // 1 = AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK
    cc_process_loopback_params ProcessLoopbackParams;
} cc_activation_params;

typedef HRESULT (WINAPI *cc_activate_fn)(LPCWSTR, REFIID, PROPVARIANT*, IActivateAudioInterfaceCompletionHandler*, IActivateAudioInterfaceAsyncOperation**);

static const IID cc_IID_IUnknown          = {0x00000000, 0x0000, 0x0000, {0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}};
static const IID cc_IID_IAgileObject      = {0x94ea2b94, 0xe9cc, 0x49e0, {0xc0, 0xff, 0xee, 0x64, 0xca, 0x8f, 0x5b, 0x90}};
static const IID cc_IID_CompletionHandler = {0x41D949AB, 0x9862, 0x444A, {0x80, 0xF6, 0xC2, 0x61, 0x33, 0x4D, 0xA5, 0xEB}};
static const IID cc_IID_IAudioClient      = {0x1CB9AD4C, 0xDBFA, 0x4c32, {0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}};
static const IID cc_IID_IAudioCapture     = {0xC8ADBD64, 0xE71E, 0x48a0, {0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}};

// ── The completion handler ActivateAudioInterfaceAsync calls back on ──
typedef struct {
    IActivateAudioInterfaceCompletionHandler iface;
    LONG   refs;
    HANDLE done;
} cc_handler;

static HRESULT STDMETHODCALLTYPE h_QueryInterface(IActivateAudioInterfaceCompletionHandler* This, REFIID riid, void** out) {
    if (IsEqualIID(riid, &cc_IID_IUnknown) || IsEqualIID(riid, &cc_IID_CompletionHandler) || IsEqualIID(riid, &cc_IID_IAgileObject)) {
        *out = This;
        InterlockedIncrement(&((cc_handler*)This)->refs);
        return S_OK;
    }
    *out = NULL;
    return E_NOINTERFACE;
}
static ULONG STDMETHODCALLTYPE h_AddRef(IActivateAudioInterfaceCompletionHandler* This) {
    return (ULONG)InterlockedIncrement(&((cc_handler*)This)->refs);
}
static ULONG STDMETHODCALLTYPE h_Release(IActivateAudioInterfaceCompletionHandler* This) {
    // Never freed: it's a static, reused for every capture.
    return (ULONG)InterlockedDecrement(&((cc_handler*)This)->refs);
}
static HRESULT STDMETHODCALLTYPE h_ActivateCompleted(IActivateAudioInterfaceCompletionHandler* This, IActivateAudioInterfaceAsyncOperation* op) {
    (void)op;
    SetEvent(((cc_handler*)This)->done);
    return S_OK;
}
static IActivateAudioInterfaceCompletionHandlerVtbl h_vtbl = {h_QueryInterface, h_AddRef, h_Release, h_ActivateCompleted};
static cc_handler handler = {{&h_vtbl}, 1, NULL};

// ── Ring buffer: interleaved stereo int16, one second ──
#define RB_FRAMES 48000
static short rb_data[RB_FRAMES * 2];
static int rb_read, rb_count;
static CRITICAL_SECTION rb_lock;
static int rb_lock_ready;

static void rb_push(const short* frames, int n) {
    EnterCriticalSection(&rb_lock);
    for (int i = 0; i < n; i++) {
        if (rb_count == RB_FRAMES) { // full: drop the oldest
            rb_read = (rb_read + 1) % RB_FRAMES;
            rb_count--;
        }
        int w = (rb_read + rb_count) % RB_FRAMES;
        if (frames) {
            rb_data[w * 2] = frames[i * 2];
            rb_data[w * 2 + 1] = frames[i * 2 + 1];
        } else {
            rb_data[w * 2] = rb_data[w * 2 + 1] = 0;
        }
        rb_count++;
    }
    LeaveCriticalSection(&rb_lock);
}

// ── The capture thread ──
static HANDLE cap_thread, cap_ready;
static volatile LONG cap_stop;
static volatile LONG cap_running;
static DWORD cap_pid;
static int cap_exclude;
static char cap_err[256];

static void fail(const char* step, HRESULT hr) {
    snprintf(cap_err, sizeof cap_err, "%s (Windows error 0x%08lX)", step, (unsigned long)hr);
}

static DWORD WINAPI capture_main(LPVOID arg) {
    (void)arg;
    IAudioClient* client = NULL;
    IAudioCaptureClient* capture = NULL;
    IActivateAudioInterfaceAsyncOperation* op = NULL;
    IUnknown* activated = NULL;
    HANDLE ev = NULL;
    HRESULT hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    int com = SUCCEEDED(hr);

    HMODULE mmdev = LoadLibraryW(L"Mmdevapi.dll");
    cc_activate_fn activate = mmdev ? (cc_activate_fn)(void*)GetProcAddress(mmdev, "ActivateAudioInterfaceAsync") : NULL;
    if (!activate) {
        fail("this version of Windows can't capture sound from single programs", E_NOTIMPL);
        goto done;
    }

    cc_activation_params params;
    memset(&params, 0, sizeof params);
    params.ActivationType = 1;
    params.ProcessLoopbackParams.TargetProcessId = cap_pid;
    params.ProcessLoopbackParams.ProcessLoopbackMode = cap_exclude ? 1 : 0;
    PROPVARIANT pv;
    memset(&pv, 0, sizeof pv);
    pv.vt = VT_BLOB;
    pv.blob.cbSize = sizeof params;
    pv.blob.pBlobData = (BYTE*)&params;

    ResetEvent(handler.done);
    hr = activate(L"VAD\\Process_Loopback", &cc_IID_IAudioClient, &pv, &handler.iface, &op);
    if (FAILED(hr)) {
        fail("couldn't start capturing sound", hr);
        goto done;
    }
    if (WaitForSingleObject(handler.done, 5000) != WAIT_OBJECT_0) {
        fail("Windows didn't answer the request to capture sound", E_FAIL);
        goto done;
    }
    HRESULT result = E_FAIL;
    hr = IActivateAudioInterfaceAsyncOperation_GetActivateResult(op, &result, &activated);
    if (FAILED(hr) || FAILED(result) || !activated) {
        fail("Windows refused to capture sound (it needs Windows 10 version 2004 or later)", FAILED(hr) ? hr : result);
        goto done;
    }
    hr = IUnknown_QueryInterface(activated, &cc_IID_IAudioClient, (void**)&client);
    if (FAILED(hr)) { fail("couldn't open the sound capture", hr); goto done; }

    // 48 kHz stereo 16-bit; Windows converts whatever's playing to this.
    WAVEFORMATEX wf;
    memset(&wf, 0, sizeof wf);
    wf.wFormatTag = WAVE_FORMAT_PCM;
    wf.nChannels = 2;
    wf.nSamplesPerSec = 48000;
    wf.wBitsPerSample = 16;
    wf.nBlockAlign = 4;
    wf.nAvgBytesPerSec = 48000 * 4;
    hr = IAudioClient_Initialize(client, AUDCLNT_SHAREMODE_SHARED,
        AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK | AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY,
        200000 /* 20 ms */, 0, &wf, NULL);
    if (FAILED(hr)) { fail("couldn't set up the sound capture", hr); goto done; }
    ev = CreateEventW(NULL, FALSE, FALSE, NULL);
    hr = IAudioClient_SetEventHandle(client, ev);
    if (FAILED(hr)) { fail("couldn't set up the sound capture", hr); goto done; }
    hr = IAudioClient_GetService(client, &cc_IID_IAudioCapture, (void**)&capture);
    if (FAILED(hr)) { fail("couldn't set up the sound capture", hr); goto done; }
    hr = IAudioClient_Start(client);
    if (FAILED(hr)) { fail("couldn't start the sound capture", hr); goto done; }

    InterlockedExchange(&cap_running, 1);
    SetEvent(cap_ready);
    while (!cap_stop) {
        WaitForSingleObject(ev, 100);
        UINT32 packet = 0;
        while (!cap_stop && SUCCEEDED(IAudioCaptureClient_GetNextPacketSize(capture, &packet)) && packet > 0) {
            BYTE* data = NULL;
            UINT32 frames = 0;
            DWORD flags = 0;
            if (FAILED(IAudioCaptureClient_GetBuffer(capture, &data, &frames, &flags, NULL, NULL))) break;
            rb_push((flags & AUDCLNT_BUFFERFLAGS_SILENT) ? NULL : (const short*)data, (int)frames);
            IAudioCaptureClient_ReleaseBuffer(capture, frames);
        }
    }
    IAudioClient_Stop(client);

done:
    InterlockedExchange(&cap_running, 0);
    SetEvent(cap_ready); // (if it never got going, this reports the failure)
    if (capture) IAudioCaptureClient_Release(capture);
    if (client) IAudioClient_Release(client);
    if (activated) IUnknown_Release(activated);
    if (op) IActivateAudioInterfaceAsyncOperation_Release(op);
    if (ev) CloseHandle(ev);
    if (mmdev) FreeLibrary(mmdev);
    if (com) CoUninitialize();
    return 0;
}

int sa_start(unsigned int pid, int exclude, char* err, int errlen) {
    sa_stop();
    if (!rb_lock_ready) {
        InitializeCriticalSection(&rb_lock);
        handler.done = CreateEventW(NULL, FALSE, FALSE, NULL);
        rb_lock_ready = 1;
    }
    EnterCriticalSection(&rb_lock);
    rb_read = rb_count = 0;
    LeaveCriticalSection(&rb_lock);
    cap_pid = pid;
    cap_exclude = exclude;
    cap_stop = 0;
    cap_err[0] = 0;
    cap_ready = CreateEventW(NULL, TRUE, FALSE, NULL);
    cap_thread = CreateThread(NULL, 0, capture_main, NULL, 0, NULL);
    if (!cap_thread) {
        snprintf(err, errlen, "couldn't start the sound capture thread");
        CloseHandle(cap_ready);
        cap_ready = NULL;
        return -1;
    }
    WaitForSingleObject(cap_ready, 8000);
    if (!cap_running) {
        snprintf(err, errlen, "%s", cap_err[0] ? cap_err : "the sound capture didn't start");
        sa_stop();
        return -1;
    }
    return 0;
}

int sa_read(float* out, int frames) {
    if (!rb_lock_ready) return 0;
    EnterCriticalSection(&rb_lock);
    int n = frames < rb_count ? frames : rb_count;
    for (int i = 0; i < n; i++) {
        int r = (rb_read + i) % RB_FRAMES;
        out[i * 2] = rb_data[r * 2] / 32768.0f;
        out[i * 2 + 1] = rb_data[r * 2 + 1] / 32768.0f;
    }
    rb_read = (rb_read + n) % RB_FRAMES;
    rb_count -= n;
    LeaveCriticalSection(&rb_lock);
    return n;
}

int sa_available(void) {
    if (!rb_lock_ready) return 0;
    EnterCriticalSection(&rb_lock);
    int n = rb_count;
    LeaveCriticalSection(&rb_lock);
    return n;
}

void sa_stop(void) {
    if (cap_thread) {
        InterlockedExchange(&cap_stop, 1);
        WaitForSingleObject(cap_thread, 3000);
        CloseHandle(cap_thread);
        cap_thread = NULL;
    }
    if (cap_ready) {
        CloseHandle(cap_ready);
        cap_ready = NULL;
    }
    cap_running = 0;
}
