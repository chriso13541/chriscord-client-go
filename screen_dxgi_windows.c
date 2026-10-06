// Which graphics adapter, and which output on it, shows a given monitor —
// what FFmpeg's Desktop Duplication source (ddagrab) needs to capture it
// (it counts outputs per adapter, so a laptop's external screen on the
// NVIDIA card is output 0 of adapter 1, say). DXGI is loaded at run time.

#define COBJMACROS
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <dxgi.h>
#include <stdint.h>

#include "screen_dxgi_windows.h"

static const IID cc_IID_IDXGIFactory1 = {0x770aae78, 0xf26f, 0x4dba, {0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}};

typedef HRESULT (WINAPI *cc_create_factory_fn)(REFIID, void**);

int dxgi_find_output(unsigned long long hmonitor, int* adapter_out, int* output_out, unsigned int* vendor_out, int* same_vendor_out) {
    HMODULE dll = LoadLibraryW(L"dxgi.dll");
    if (!dll) return -1;
    cc_create_factory_fn create = (cc_create_factory_fn)(void*)GetProcAddress(dll, "CreateDXGIFactory1");
    IDXGIFactory1* factory = NULL;
    int found = -1;
    unsigned int vendors[16];
    int nadapters = 0;
    if (!create || FAILED(create(&cc_IID_IDXGIFactory1, (void**)&factory)) || !factory) {
        FreeLibrary(dll);
        return -1;
    }
    for (UINT a = 0; found < 0; a++) {
        IDXGIAdapter1* adapter = NULL;
        if (IDXGIFactory1_EnumAdapters1(factory, a, &adapter) != S_OK || !adapter) break;
        DXGI_ADAPTER_DESC1 ad;
        unsigned int vendor = 0;
        if (SUCCEEDED(IDXGIAdapter1_GetDesc1(adapter, &ad))) vendor = ad.VendorId;
        if (nadapters < 16) vendors[nadapters++] = vendor;
        for (UINT o = 0; found < 0; o++) {
            IDXGIOutput* output = NULL;
            if (IDXGIAdapter1_EnumOutputs(adapter, o, &output) != S_OK || !output) break;
            DXGI_OUTPUT_DESC desc;
            if (SUCCEEDED(IDXGIOutput_GetDesc(output, &desc)) && (unsigned long long)(uintptr_t)desc.Monitor == hmonitor) {
                *adapter_out = (int)a;
                *output_out = (int)o;
                *vendor_out = vendor;
                found = 0;
            }
            IDXGIOutput_Release(output);
        }
        IDXGIAdapter1_Release(adapter);
    }
    // Keep counting adapters, to know if another shares this vendor.
    if (found == 0) {
        for (UINT a = (UINT)nadapters; a < 16; a++) {
            IDXGIAdapter1* adapter = NULL;
            if (IDXGIFactory1_EnumAdapters1(factory, a, &adapter) != S_OK || !adapter) break;
            DXGI_ADAPTER_DESC1 ad;
            vendors[nadapters++] = SUCCEEDED(IDXGIAdapter1_GetDesc1(adapter, &ad)) ? ad.VendorId : 0;
            IDXGIAdapter1_Release(adapter);
        }
        int same = 0;
        for (int i = 0; i < nadapters; i++) if (vendors[i] == *vendor_out) same++;
        *same_vendor_out = same;
    }
    IDXGIFactory1_Release(factory);
    FreeLibrary(dll);
    return found;
}
