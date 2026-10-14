# Embedded archive reader support

The embedded engine is official **7-Zip 26.04**, built with WASI SDK 34.0 and
executed by wazero. All **60 container handlers** in the upstream full archive
bundle are registered. The native CLI's additional `Hash` handler is a checksum
file tool, not an archive reader. No external decompressor is needed at runtime.

Registration is not a claim that every filesystem variant, damaged input or
compression/encryption combination has been tested. Upstream limitations and the
indexing limits below still apply. This matrix was audited against the upstream
handler registry, volume callbacks and crypto code on 2026-10-13.

## Complete format registry

The results below come from native 7-Zip 26.04 versus embedded decoding,
with output lengths and SHA-256 compared. A matching result with an upstream
warning remains an incomplete index; it is not silently upgraded to complete.

| Handler | Registered extensions | Runtime validation |
|---|---|---|
| 7z | 7z | 80 sample(s), decoded content matches |
| APFS | apfs img | 2 sample(s), decoded content matches; metadata warnings preserved |
| APM | apm | 1 sample(s), decoded content matches |
| Ar | ar a deb udeb lib | 2 sample(s), decoded content matches |
| Arj | arj | 16 sample(s), decoded content matches |
| Base64 | b64 | 1 sample(s), decoded content matches |
| COFF | obj | 1 sample(s), decoded content matches |
| Cab | cab | 13 sample(s), decoded content matches |
| Chm | chm chi chq chw | 1 sample(s), decoded content matches |
| Compound | msi msp msm doc xls ppt aaf | 1 sample(s), decoded content matches |
| Cpio | cpio | 9 sample(s), decoded content matches |
| CramFS | cramfs | 1 sample(s), decoded content matches |
| Dmg | dmg | 1 sample(s), decoded content matches; repaired diagnostic copy |
| ELF | elf | 1 sample(s), decoded content matches |
| Ext | ext ext2 ext3 ext4 img | 3 sample(s), decoded content matches |
| FAT | fat img | 1 sample(s), decoded content matches |
| FLV | flv | 1 sample(s), decoded content matches |
| GPT | gpt mbr | 1 sample(s), decoded content matches |
| HFS | hfs hfsx | 1 sample(s), decoded content matches |
| Hxs | hxs hxi hxr hxq hxw lit | No representative sample; registered only |
| IHex | ihex | 1 sample(s), decoded content matches |
| Iso | iso img | 18 sample(s), decoded content matches |
| LP | lpimg img | 1 sample(s), decoded content matches |
| Lzh | lzh lha | 11 sample(s), decoded content matches |
| MBR | mbr | 1 sample(s), decoded content matches |
| MachO | macho | 1 sample(s), decoded content matches |
| MsLZ | mslz | 1 sample(s), decoded content matches |
| Mub | mub | 1 sample(s), decoded content matches |
| NTFS | ntfs img | 1 sample(s), decoded content matches |
| Nsis | nsis | 1 sample(s), decoded content matches |
| PE | exe dll sys | 1 sample(s), decoded content matches |
| Ppmd | pmd | 1 sample(s), decoded content matches |
| QCOW | qcow qcow2 qcow2c | 1 sample(s), decoded content matches |
| Rar | rar r00 | 22 sample(s), decoded content matches |
| Rar5 | rar r00 | 23 sample(s), decoded content matches |
| Rpm | rpm | 2 sample(s), decoded content matches |
| SWF | swf | 1 sample(s), decoded content matches |
| SWFc | swf | 1 sample(s), decoded content matches |
| Sparse | simg img | 1 sample(s), decoded content matches |
| Split | 001 | 15 numeric/alphabetic split + encryption combinations; decoded contents checked |
| SquashFS | squashfs | 6 sample(s), decoded content matches |
| TE | te | 1 sample(s), decoded content matches |
| UEFIc | scap | 1 sample(s), decoded content matches |
| UEFIf | uefif | 1 sample(s), decoded content matches |
| Udf | udf iso img | 2 sample(s), decoded content matches |
| VDI | vdi | 1 sample(s), decoded content matches |
| VHD | vhd | 2 sample(s), decoded content matches |
| VHDX | vhdx avhdx | 2 sample(s), decoded content matches |
| VMDK | vmdk | 1 sample(s), decoded content matches |
| Xar | xar pkg xip | 9 sample(s), decoded content matches |
| Z | z taz | 40 sample(s), decoded content matches |
| bzip2 | bz2 bzip2 tbz2 tbz | 1 sample(s), decoded content matches |
| gzip | gz gzip tgz tpz apk | 2 sample(s), decoded content matches |
| lzma | lzma | 3 sample(s), decoded content matches |
| lzma86 | lzma86 | 1 sample(s), decoded content matches |
| tar | tar ova | 2 sample(s), decoded content matches |
| wim | wim swm esd ppkg | 2 sample(s), decoded content matches |
| xz | xz txz | 1 sample(s), decoded content matches |
| zip | zip z01 zipx jar xpi odt ods docx xlsx epub ipa apk appx | 64 sample(s), decoded content matches |
| zstd | zst tzst | 4 sample(s), decoded content matches |

## Automatic indexing

Deep search reconstructs files and directories that the user packed, through
successive archive layers. For example, `backup.zip!/game.rar!/OST.zip!/track.flac`
exposes the track name. Recursion stops at the music file. The decoder registry
above describes low-level reading capabilities, not the default indexing scope.

The same small allowlist applies at the source root and in nested archives:

- Archives: `.7z`, `.zip`, `.zipx`, `.rar`, `.tar`, `.cpio`, `.arj`, `.lha`, `.lzh`, `.pmd`.
- Compressed streams: `.gz`, `.gzip`, `.bz2`, `.bzip2`, `.xz`, `.zst`, `.zstd`,
  `.lzma`, `.lzma86`, `.Z`.
- Combined names: `.tgz`, `.taz`, `.tbz`, `.tbz2`, `.txz`, `.tzst` and ordinary
  combinations such as `.tar.gz`.
- First numeric/alphabetic split volumes of these archives, plus names without an
  earlier extension such as `backup.001`. Known non-archive names such as
  `setup.exe.001` remain opaque. Continuation pieces are resolved as siblings.

Suffix matching is case-insensitive. After opening, only the corresponding
archive/compression handlers may expose entries; a misleading `.zip` suffix on
an executable, media file or image does not expose its implementation details.
Unknown or application-specific suffixes are not sniffed for archive signatures.
An archive deliberately named as an ordinary ZIP is indexed as that ZIP.

Programs, libraries, application and installation packages (APK, JAR, MSI, CAB,
DEB/RPM, NSIS, self-extracting EXE, etc.), Office and other documents, ebooks,
help files, media, project files, firmware and disk images are leaf files.
No inner XML, bytecode, media frames, machine-code sections or image partitions
are exposed. Their original bytes are still fully backed up. An explicit
`document.pdf.gz` can expose `document.pdf`, which then remains a leaf file.

There is no automatic index policy editor, per-format CLI switch, or fallback to
a different decoder. The full embedded reader remains self-contained and tested,
while this automatic selection rule stays deliberately narrow.

The POSIX 7-Zip engine can return legacy filename bytes that are not UTF-8.
The reader preserves those bytes, including nested archive and volume names.
The catalog uses the same reversible percent escaping as native Linux filenames,
so invalid UTF-8 does not reject an entire archive or merge distinct names.
It does not guess an unrecorded code page; UTF-8 and archive-provided Unicode names
remain readable normally.

## Decoder volume capabilities

ZIP, RAR and raw split archives below participate in automatic indexing. CAB, WIM
and virtual-disk capabilities are low-level reader coverage only; those files
remain opaque in `stage plan`.

| Layout | Implementation and validation |
|---|---|
| Raw numeric split `.001/.002`, `.01/.02`, `.00/.01` | Upstream Split handler provides a seekable combined stream; tested with plain/encrypted 7z and encrypted ZIP |
| Raw alphabetic split `.aa/.ab`, `.aaa/.aab` | Same upstream stream; tested with plain/encrypted 7z and encrypted ZIP |
| ZIP `.z01/.z02/.../.zip` | Native ZIP volume callback; full extraction and nested indexing tested; start with `.zip` |
| RAR `.partNN.rar` | RAR3/4 and RAR5 handlers; extraction tested |
| Old RAR `.rar/.r00/.r01` | Extraction and nested indexing tested; upstream also handles the later `.s00` naming rollover |
| CAB sets | Names come from CAB headers; low-level extraction tested |
| WIM `.swm`, `name2.swm`, ... | Names/count come from WIM; three-volume low-level extraction tested |
| VMDK flat extents | Selected sibling extent opened through upstream volume callback; decoded bytes compared with native 7-Zip |
| VHD/VHDX parents; MsLZ companions; TAR references | Callbacks wired; missing-parent VHDX rejection exercised, successful multi-file parent/reference chains not yet qualified |
| ARJ `.arj/.a01/...` | Individual volume contents decode, but upstream does not join them; taper detects the fragment position and explicitly marks the index partial |

Every volume/companion must be part of the selected source files in the same
directory, or an unambiguous sibling member of the enclosing archive. Paths never
become unrestricted host filesystem reads. Common nested archives and their
volumes are extracted together in archive order, preserving solid-stream access.
An unusually named companion is materialized on demand from the local parent.
No whole split archive is concatenated into a new temporary file.

Start with the primary volume: continuation-only selections are not a usable
archive index. Missing volumes, read errors and upstream metadata warnings, including
nonzero data after an embedded archive, must not be presented as a complete index.
At the low-level reader layer, WIM can list absent resources with zero sizes; its
volume completeness is not inferred from listing alone. It is not auto-indexed.
Raw split streams have no part-count manifest: a missing final piece cannot always
be detected when the underlying format has no completeness/checksum information.

## Encryption

| Scheme | Status |
|---|---|
| 7z AES-256, including encrypted headers | Tested, including split streams and wrong/missing passwords |
| ZIP traditional ZipCrypto | Tested |
| ZIP WinZip AES-128/192/256 | Tested |
| ZIP PKWARE strong AES-256 | Tested; upstream code also handles AES-128/192 |
| RAR 2.0 legacy encryption | Tested |
| RAR3/4 AES-128, encrypted headers and solid data | Tested |
| RAR5 AES-256, encrypted headers and data | Tested |
| RAR before 2.0 encryption; ARJ encryption | Unsupported by upstream extraction |
| ZIP encrypted central directory; PKWARE certificate/recipient modes and non-AES strong encryption | Unsupported; encrypted-directory fixture is retained as an incomplete-index regression |
| Encrypted APFS/DMG and encrypted virtual disk formats | No general decryption support in these handlers |

A container with visible encrypted filenames is still `locked` until a password
has been checked by decoding an encrypted member (the smallest suitable member
is chosen). Header-only success does not silently validate a data password.
Passwords are UTF-8; legacy OEM-codepage passwords can be platform-dependent.
The simple interface supplies one password per container, inherited by nested
containers unless overridden. Archives using different passwords for different
members may only be partly indexable; there is no per-member password manager.
Passwords are saved only with `stage unlock --remember PATH`, inside the encrypted
catalog, and only for an encrypted container that accepted that password.

## Local validation and limits

The 2026-10-13 audit attempted 504 local inputs. **373 successful native/embedded
comparisons cover 58 handlers**, including compressed filesystems and disk images.
Decoded lengths and SHA-256 match over **233,409,961 bytes**. Split is additionally
validated by its dedicated volume tests, so **59 of 60 handlers** have runtime
evidence. **Hxs remains unqualified** because no reliable sample was available.
This does not prove every variant or damaged input is supported.

The retained corpus has **93 cases** (47 codec/encryption cases, 45
format representatives and one partial ARJ-volume regression), with native output lengths, hashes and known warning
states. Additional tests cover split names, wrong/missing passwords, nested
volume sets, legacy filenames, trailing data, nested user archives stopping at leaf files,
misleading suffixes and offline browsing. Fixtures are public or synthetic, compressed for repository size, with
provenance and licenses in `internal/archive/testdata`. Private diagnostic files
were used only outside the repository.

A further **121 negative or unusual inputs** exercised rejection and partial
reads: 109 were rejected during open/extraction, 11 remained readable with visible
metadata warnings, and one valid MBR was recognized despite the native CLI's
automatic detection declining it. No time-limit abort or Wasm trap occurred in
this sample set. Large fixtures exceeding the audit's output budget were not
counted as failures of the implementation.

Specific limitations confirmed against the native engine:

- LZ4-compressed SquashFS and Zstandard-compressed QCOW in the tested samples
  are unsupported; gzip/LZMA/LZO/XZ/Zstandard SquashFS and ordinary QCOW decoded.
- Encrypted APFS is not generally supported. The unencrypted APFS samples decode
  but retain upstream metadata warnings.
- Missing parent images or selected extents can prevent complete extraction.
- Kuaizip KZ is not supported. An embedded RAR notice may be listed, but the
  trailing-data warning keeps that result partial.
- Structural image fragments such as a protective MBR can reference bytes outside
  the fragment. A readable parent filesystem does not make such a fragment a
  self-contained disk image.

The guest has no filesystem preopens. Each module has a 1 GiB linear-memory cap;
all input callbacks read at most 1 MiB. Indexing is limited to depth 16, one million
entries, 64 GiB cumulative expanded output per outer container, a 1 GiB scratch
reserve and 30 minutes per container. Limits produce visible incomplete states.
Parent modules are closed before normal recursion; uncommon lazy dependencies
can temporarily keep a child and a reopened parent alive together. These limits
are not a promise that total process RSS is exactly one guest memory cap.

A local qualification exercised bounded file reads, ZIP decoding and SHA-256 over
128 MiB of decoded data: **126.91 MB/s**, maximum read **1 MiB**, **123,881,848** Go
allocated bytes, **52,210** allocations, **19** collections and **58,592 KiB** peak
process RSS (including runtime setup). This is one synthetic workload, not a
general decoding-speed guarantee. Indexing runs during local planning and is not
part of the tape streaming path. No physical tape was accessed for these checks.
