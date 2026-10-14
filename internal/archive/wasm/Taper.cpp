#include "../../../Common/Common.h"
#include "../../../Common/MyWindows.h"
#include "../../../Common/MyInitGuid.h"
#include "../../../Common/UTFConvert.h"
#include "../../UI/Common/OpenArchive.h"
#include "../../IPassword.h"
#include <stdint.h>
#include <initializer_list>

extern "C" {
__attribute__((import_module("taper"), import_name("read"))) int
host_read(uint32_t, uint64_t, void *, uint32_t);
__attribute__((import_module("taper"), import_name("size"))) int64_t
host_size(uint32_t);
__attribute__((import_module("taper"), import_name("volume"))) int
host_volume(const char *, uint32_t);
__attribute__((import_module("taper"), import_name("item"))) int
host_item(uint32_t, const char *, uint32_t, int64_t, uint32_t);
__attribute__((import_module("taper"), import_name("format"))) void
host_format(const char *, uint32_t);
__attribute__((import_module("taper"), import_name("status"))) void
host_status(uint32_t, const char *, uint32_t);
}

class Input final : public IInStream, public CMyUnknownImp {
  Z7_COM_UNKNOWN_IMP_2(IInStream, ISequentialInStream)
  uint32_t id;
  uint64_t pos;

public:
  Input(uint32_t n) : id(n), pos(0) {}
  Z7_COM7F_IMF(Read(void *p, UInt32 n, UInt32 *used)) {
    int r = host_read(id, pos, p, n);
    if (used)
      *used = r < 0 ? 0 : r;
    if (r < 0)
      return E_FAIL;
    pos += r;
    return S_OK;
  }
  Z7_COM7F_IMF(Seek(Int64 off, UInt32 whence, UInt64 *result)) {
    int64_t base = whence == 0 ? 0 : whence == 1 ? pos : host_size(id);
    if (whence > 2 || base < 0)
      return E_INVALIDARG;
    if (off < 0) {
      uint64_t delta = (uint64_t)(-(off + 1)) + 1;
      if ((uint64_t)base < delta)
        return E_INVALIDARG;
      pos = (uint64_t)base - delta;
    } else {
      if ((uint64_t)base > (uint64_t)INT64_MAX - (uint64_t)off)
        return E_INVALIDARG;
      pos = (uint64_t)base + (uint64_t)off;
    }
    if (result)
      *result = pos;
    return S_OK;
  }
};
class OpenCallback final : public IArchiveOpenCallback,
                           public IArchiveOpenVolumeCallback,
                           public ICryptoGetTextPassword,
                           public CMyUnknownImp {
public:
  Z7_COM_UNKNOWN_IMP_3(IArchiveOpenCallback, IArchiveOpenVolumeCallback,
                       ICryptoGetTextPassword)
public:
  UString Name, Password;
  bool Provided = false, Asked = false;
  Z7_COM7F_IMF(SetTotal(const UInt64 *, const UInt64 *)) { return S_OK; }
  Z7_COM7F_IMF(SetCompleted(const UInt64 *, const UInt64 *)) { return S_OK; }
  Z7_COM7F_IMF(GetProperty(PROPID id, PROPVARIANT *v)) {
    NWindows::NCOM::CPropVariant p;
    if (id == kpidName)
      p = Name;
    if (id == kpidSize)
      p = (UInt64)host_size(0);
    if (id == kpidIsDir)
      p = false;
    return p.Detach(v);
  }
  Z7_COM7F_IMF(GetStream(const wchar_t *name, IInStream **stream)) {
    *stream = nullptr;
    AString s;
    ConvertUnicodeToUTF8(name, s);
    int id = host_volume(s.Ptr(), s.Len());
    if (id < 0)
      return S_FALSE;
    CMyComPtr<IInStream> p = new Input(id);
    *stream = p.Detach();
    return S_OK;
  }
  Z7_COM7F_IMF(CryptoGetTextPassword(BSTR *out)) {
    Asked = true;
    if (!Provided)
      return E_ABORT;
    return StringToBstr(Password, out);
  }
};
extern "C" {
__attribute__((import_module("taper"), import_name("write"))) int
host_write(uint32_t, const void *, uint32_t);
__attribute__((import_module("taper"), import_name("finish"))) void
host_finish(uint32_t, uint32_t);
}
class Output final : public ISequentialOutStream, public CMyUnknownImp {
  Z7_COM_UNKNOWN_IMP_1(ISequentialOutStream)
  uint32_t id;

public:
  explicit Output(uint32_t i) : id(i) {}
  Z7_COM7F_IMF(Write(const void *p, UInt32 size, UInt32 *used)) {
    int n = host_write(id, p, size);
    if (used)
      *used = n < 0 ? 0 : n;
    return n < 0 ? E_ABORT : S_OK;
  }
};
class ExtractCallback final : public IArchiveExtractCallback,
                              public ICryptoGetTextPassword,
                              public CMyUnknownImp {
  Z7_COM_UNKNOWN_IMP_2(IArchiveExtractCallback, ICryptoGetTextPassword)
  OpenCallback *open;
  uint32_t current = 0;

public:
  bool Failed = false;
  explicit ExtractCallback(OpenCallback *p) : open(p) {}
  Z7_COM7F_IMF(SetTotal(UInt64)) { return S_OK; }
  Z7_COM7F_IMF(SetCompleted(const UInt64 *)) { return S_OK; }
  Z7_COM7F_IMF(GetStream(UInt32 index, ISequentialOutStream **stream,
                         Int32 mode)) {
    current = index;
    *stream = nullptr;
    if (mode == NArchive::NExtract::NAskMode::kExtract) {
      CMyComPtr<ISequentialOutStream> p = new Output(index);
      *stream = p.Detach();
    }
    return S_OK;
  }
  Z7_COM7F_IMF(PrepareOperation(Int32)) { return S_OK; }
  Z7_COM7F_IMF(SetOperationResult(Int32 result)) {
    host_finish(current, result);
    Failed |= result != 0;
    return S_OK;
  }
  Z7_COM7F_IMF(CryptoGetTextPassword(BSTR *pass)) {
    return open->CryptoGetTextPassword(pass);
  }
};
static CCodecs codecs;
static CArc *arc = nullptr;
static CArc *split = nullptr;
static OpenCallback *callback;
static CMyComPtr<IArchiveOpenCallback> callbackOwner;
extern "C" int taper_open(const char *name, const char *password,
                          int provided) {
  try {
    delete arc;
    delete split;
    split = nullptr;
    arc = new CArc;
    callback = new OpenCallback;
    callbackOwner = callback;
    ConvertUTF8ToUnicode(AString(name), callback->Name);
    if (provided)
      ConvertUTF8ToUnicode(AString(password), callback->Password);
    callback->Provided = provided;
    HRESULT hr = codecs.Load();
    if (hr != S_OK)
      return 3;
    CMyComPtr<IInStream> input = new Input(0);
    COpenOptions opt;
    opt.codecs = &codecs;
    opt.stream = input;
    opt.filePath = callback->Name;
    opt.callback = callback;
    opt.props = nullptr;
    opt.openType.Recursive = false;
    arc->Path = callback->Name;
    hr = arc->OpenStream(opt);
    if (hr != S_OK)
      return callback->Asked ? 2 : hr == S_FALSE ? 1 : 3;
    // Let upstream join numeric/alphabetic split streams, without copying
    // them into memory or implementing its naming rules in Go.
    if (codecs.Formats[arc->FormatIndex].Name.IsEqualTo("Split")) {
      split = arc;
      arc = new CArc;
      CMyComPtr<IInArchiveGetStream> get;
      CMyComPtr<ISequentialInStream> seq;
      CMyComPtr<IInStream> joined;
      if (split->Archive.QueryInterface(IID_IInArchiveGetStream, &get) != S_OK ||
          get->GetStream(0, &seq) != S_OK ||
          seq.QueryInterface(IID_IInStream, &joined) != S_OK)
        return 3;
      split->GetItem_Path(0, arc->Path);
      callback->Name = arc->Path;
      opt.filePath = arc->Path;
      opt.stream = joined;
      arc->IgnoreSplit = true;
      hr = arc->OpenStream(opt);
      if (hr != S_OK)
        return callback->Asked ? 2 : hr == S_FALSE ? 1 : 3;
    }
    const UString &format = codecs.Formats[arc->FormatIndex].Name;
    AString fs;
    ConvertUnicodeToUTF8(format, fs);
    host_format(fs.Ptr(), fs.Len());
    uint32_t flags = callback->Asked ? 1 : 0;
    const CArcErrorInfo &info = arc->ErrorInfo;
    if (info.IsThereErrorOrWarning()) flags |= 2;
    AString problem;
    if (info.NeedTailWarning()) problem = "data after end of archive";
    for (const UString *message : {&info.ErrorMessage, &info.WarningMessage}) {
      if (!message->IsEmpty()) {
        AString text;
        ConvertUnicodeToUTF8(*message, text);
        if (!problem.IsEmpty()) problem += "; ";
        problem += text;
      }
    }
    UInt32 count = 0;
    bool splitArj = false;
    if (arc->Archive->GetNumberOfItems(&count) != S_OK)
      return 3;
    for (UInt32 i = 0; i < count; i++) {
      UString path;
      if (arc->GetItem_Path(i, path) != S_OK)
        return 3;
      AString utf;
      ConvertUnicodeToUTF8(path, utf);
      UInt64 size = 0;
      bool defined = false;
      if (arc->GetItem_Size(i, size, defined) != S_OK)
        return 3;
      bool dir = false, encrypted = false;
      Archive_IsItem_Dir(arc->Archive, i, dir);
      Archive_GetItemBoolProp(arc->Archive, i, kpidEncrypted, encrypted);
      // ARJ exposes each volume fragment as a member, without joining it or
      // reporting a warning. Position is defined only for such fragments.
      if (format.IsEqualTo("Arj")) {
        NWindows::NCOM::CPropVariant position;
        arc->Archive->GetProperty(i, kpidPosition, &position);
        splitArj |= position.vt != VT_EMPTY;
      }
      NWindows::NCOM::CPropVariant link;
      arc->Archive->GetProperty(i, kpidSymLink, &link);
      bool isLink = link.vt == VT_BSTR;
      if (!isLink) {
        link.Clear();
        arc->Archive->GetProperty(i, kpidHardLink, &link);
        isLink = link.vt == VT_BSTR;
      }
      if (host_item(i, utf.Ptr(), utf.Len(), defined ? (int64_t)size : -1,
                    (dir ? 1 : 0) | (encrypted ? 2 : 0) | (isLink ? 4 : 0)))
        return 4;
    }
    if (splitArj) {
      flags |= 2;
      if (!problem.IsEmpty()) problem += "; ";
      problem += "split ARJ member; volumes are not combined";
    }
    host_status(flags, problem.Ptr(), problem.Len());
    return 0;
  } catch (...) {
    return 3;
  }
}

extern "C" int taper_extract(const UInt32 *indices, UInt32 count) {
  try {
    if (!arc || !arc->Archive)
      return 3;
    ExtractCallback *cb = new ExtractCallback(callback);
    CMyComPtr<IArchiveExtractCallback> owner = cb;
    HRESULT hr = arc->Archive->Extract(indices, count, 0, owner);
    if (hr != S_OK || cb->Failed)
      return callback->Asked ? 2 : 3;
    return 0;
  } catch (...) {
    return 3;
  }
}

#include <errno.h>
#include <string.h>
#include <sys/sysinfo.h>
extern "C" pid_t getppid(void) { return 0; }
extern "C" mode_t umask(mode_t) { return 077; }
extern "C" int chown(const char *, uid_t, gid_t) {
  errno = ENOTSUP;
  return -1;
}
extern "C" int lchown(const char *, uid_t, gid_t) {
  errno = ENOTSUP;
  return -1;
}
extern "C" int sysinfo(struct sysinfo *s) {
  memset(s, 0, sizeof(*s));
  s->mem_unit = 1;
  s->totalram = 1U << 30;
  return 0;
}
