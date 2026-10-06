/*
 * chriscord's additions to FFmpeg's gfxcapture (Windows Graphics Capture):
 *
 *  1. cc_request_borderless: lets display_border=0 actually hide the yellow
 *     capture border on Windows 11.
 *  2. cc_follow_cursor: shows the mouse pointer in the capture only while
 *     it's actually showing on screen — a video player that hides it during
 *     playback hides it from the stream too. (Windows Graphics Capture draws
 *     it regardless; Desktop Duplication, ddagrab, already follows it.)
 *
 * About 1:
 * Windows only honours IsBorderRequired = false once the capturing program
 * has asked for it with GraphicsCaptureAccess.RequestAccessAsync(Borderless);
 * otherwise the setting "succeeds" and is ignored. FFmpeg 9.0 doesn't ask,
 * so build-ffmpeg.sh copies this file next to vsrc_gfxcapture_winrt.cpp and
 * calls cc_request_borderless() just before the border is turned off.
 * (Desktop programs that aren't installed from the Store are allowed without
 * a prompt.) MinGW's headers don't have GraphicsCaptureAccess yet, so its
 * interface is declared here.
 */
#ifndef CC_GFXCAPTURE_BORDERLESS_H
#define CC_GFXCAPTURE_BORDERLESS_H

#include <windows.h>
#include <roapi.h>

MIDL_INTERFACE("743ed370-06ec-5040-a58a-901f0f757095")
CcIGraphicsCaptureAccessStatics : public IInspectable
{
    public:
    virtual HRESULT STDMETHODCALLTYPE RequestAccessAsync(INT32 kind, IInspectable **operation) = 0;
};

/* Windows.Foundation.IAsyncInfo, declared here to stay independent of
 * which MinGW headers are installed. */
MIDL_INTERFACE("00000036-0000-0000-C000-000000000046")
CcIAsyncInfo : public IInspectable
{
    public:
    virtual HRESULT STDMETHODCALLTYPE get_Id(UINT32 *id) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_Status(INT32 *status) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_ErrorCode(HRESULT *code) = 0;
    virtual HRESULT STDMETHODCALLTYPE Cancel(void) = 0;
    virtual HRESULT STDMETHODCALLTYPE Close(void) = 0;
};

template <typename C>
static void cc_request_borderless(AVFilterContext *avctx, C *ctx)
{
    static const IID iid_statics   = { 0x743ed370, 0x06ec, 0x5040, { 0xa5, 0x8a, 0x90, 0x1f, 0x0f, 0x75, 0x70, 0x95 } };
    static const IID iid_asyncinfo = { 0x00000036, 0x0000, 0x0000, { 0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46 } };
    static const wchar_t cls[] = L"Windows.Graphics.Capture.GraphicsCaptureAccess";
    HSTRING_HEADER hdr;
    HSTRING hs = NULL;
    CcIGraphicsCaptureAccessStatics *statics = NULL;
    IInspectable *op = NULL;
    CcIAsyncInfo *info = NULL;
    INT32 status = 0;

    HRESULT hr = ctx->fn.WindowsCreateStringReference(cls, (UINT32)wcslen(cls), &hdr, &hs);
    if (SUCCEEDED(hr))
        hr = ctx->fn.RoGetActivationFactory(hs, iid_statics, (void **)&statics);
    if (FAILED(hr)) {
        av_log(avctx, AV_LOG_WARNING, "Borderless capture permission unavailable (0x%08lX)\n", hr);
        return;
    }
    hr = statics->RequestAccessAsync(0 /* GraphicsCaptureAccessKind::Borderless */, &op);
    if (SUCCEEDED(hr))
        hr = op->QueryInterface(iid_asyncinfo, (void **)&info);
    if (SUCCEEDED(hr)) {
        /* Wait (up to 3 s) for the answer, keeping this thread's messages
         * moving in case the answer is delivered through them. */
        for (int i = 0; i < 300 && SUCCEEDED(info->get_Status(&status)) && status == 0 /* Started */; i++) {
            MSG msg;
            while (PeekMessage(&msg, NULL, 0, 0, PM_REMOVE)) {
                TranslateMessage(&msg);
                DispatchMessage(&msg);
            }
            Sleep(10);
        }
        av_log(avctx, AV_LOG_VERBOSE, "Borderless capture permission requested (status %d)\n", (int)status);
    } else {
        av_log(avctx, AV_LOG_WARNING, "Borderless capture permission request failed (0x%08lX)\n", hr);
    }
    if (info)
        info->Release();
    if (op)
        op->Release();
    statics->Release();
}

/* About 2: called before each frame is fetched (on the capture thread).
 * Checks the pointer at most every 50 ms and switches the session's own
 * cursor drawing on or off to match. */
template <typename S>
static void cc_follow_cursor(AVFilterContext *avctx, S &session)
{
    static int shown_last = -1;
    static int64_t checked = 0;
    int64_t now = av_gettime_relative();
    if (now - checked < 50000)
        return;
    checked = now;

    CURSORINFO ci;
    memset(&ci, 0, sizeof(ci));
    ci.cbSize = sizeof(ci);
    int shown = 1;
    if (GetCursorInfo(&ci))
        shown = (ci.flags & CURSOR_SHOWING) && ci.hCursor;
    if (shown == shown_last)
        return;

    Microsoft::WRL::ComPtr<ABI::Windows::Graphics::Capture::IGraphicsCaptureSession2> session2;
    if (SUCCEEDED(session.As(&session2)) && SUCCEEDED(session2->put_IsCursorCaptureEnabled(shown ? 1 : 0))) {
        av_log(avctx, AV_LOG_VERBOSE, "Mouse pointer %s\n", shown ? "shown" : "hidden");
        shown_last = shown;
    }
}

#endif
