# HEIF regression fixture

`grid-rotated.heic` is synthetic test data, generated for this project. It contains
four solid color quadrants in a 3072x2048 image, split into 24 HEVC tiles of
512x512 pixels, with a 90-degree clockwise container rotation. The displayed
image is 2048x3072: blue/red above yellow/green. It contains no personal photo,
EXIF, location, or other user metadata.

Generation uses libheif's encoder:

```sh
heif-enc -q 95 --cut-tiles 512 --rotate-cw 90 \
  -p x265:pools=1 -p x265:frame-threads=1 \
  -o grid-rotated.heic quadrants.png
```

The source quadrants are RGB (230,20,20), (20,230,20), (20,20,230), and
(230,230,20), ordered top-left, top-right, bottom-left, bottom-right.
FFmpeg's first video stream exposes only a 512x512 tile; libheif reconstructs
the complete image. Tests verify dimensions and all four quadrants after resize,
JXL decoding, and JPEG fallback.

`grid-small.heic` uses the same quadrants resized to 640x480, 256x256 tiles,
and the same 90-degree rotation. Its displayed size is 480x640 and it qualifies
for keeping source bytes. `multiple.heic` has two top-level copies of that small
unrotated source, encoded without tiling. It verifies that ambiguous multi-image
containers retain their original rather than silently selecting one image.
