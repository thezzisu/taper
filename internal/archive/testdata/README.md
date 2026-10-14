# Reader fixtures

`cases.json` records SHA-256 and decoded length from native official 7-Zip 26.04
extraction (`7zz x -so -pPASSWORD FILE`). Tests require no native program.

- `plain.7z`, `encrypted.7z`, `encrypted.zip`, `aes*.zip`, `zipcrypto.zip`,
  `plain.{xz,gzip,bzip2,tar,wim}` contain synthetic `hello.txt`:
  `hello archive\n`. Generated using native 7-Zip; password `test-password`.
- `split.zip` and `.z01`–`.z03` were generated using Info-ZIP
  `zip -0 -s 64k split.zip payload.bin`, with 200,000 deterministic pseudorandom bytes.
- `image.swm`, `image2.swm`, `image3.swm` were generated with wimlib 1.14.4:
  `capture INPUT source.wim --compress=none`, then `split source.wim image.swm 0.1`.
  They contain three synthetic 70,000-byte files. No disk or personal data.
- `test_read_format_*` and `zip-encrypted-directory.zip` come from
  [libarchive's test corpus](https://github.com/libarchive/libarchive/tree/master/libarchive/test).
  The latter is `test_read_format_zip_encryption_header.zip`, password `12345678`.
  See `libarchive-LICENSE` and the golden manifest for passwords.
- `rar3-old.*`, `rar5-vols.*`, `rar202-comment-psw.rar`, `rar3-comment-hpsw.rar`,
  `rar5-encrypted.rar` and `rar5-data-encrypted.rar` come from
  [rarfile's test corpus](https://github.com/markokr/rarfile/tree/master/test/files).
  Encrypted fixtures use `password`; see `rarfile-LICENSE`.
- `test.part01.rar` and `test.part02.rar` come from
  [mholt/archives](https://github.com/mholt/archives/tree/master/testdata).
  See `archives-LICENSE`.

The fixtures are public or synthetic and contain no catalog, tape key or personal
inventory. Temporary fixture generators and audit programs are outside taper.

`format-cases.json` adds 45 format representatives and a partial ARJ-volume regression. Files under `formats/` use a
standard gzip storage wrapper, removed by the test before handing bytes to 7-Zip.
The wrapper is only to keep large sparse filesystem/disk fixtures small. Each
case checks the detected handler, decoded length, SHA-256 and warning state.
VMDK's flat extent is supplied as a sibling. `formats/sources.json` records origin
and raw SHA-256; source licenses are retained alongside it.

The APFS, VDI/VHD/VHDX and SquashFS representatives come from the corresponding
Dissect public test repositories. ARJ comes from SharpCompress; Compound from
olefile. The 7-Zip CHM and PE samples come from its official Windows distribution;
the NSIS sample is its official 3.11 installer, treated only as data. Nothing is
executed, mounted or installed during tests. All other new standalone specimens
are synthetic or from libarchive's public corpus.
