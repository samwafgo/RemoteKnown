'use strict';

// Run from any directory: node web/scripts/generate-icons.js
// Uses the existing jimp 1.6 and png-to-ico 2.1 installations; no source assets
// are changed. assets/logo.png is actually a JPEG, which Jimp detects by bytes.
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const path = require('node:path');
const { Jimp } = require('jimp');
const pngToIco = require('png-to-ico');

const webDirectory = path.resolve(__dirname, '..');
const icoSizes = [256, 48, 32, 16];

function extractTile(source) {
    assert.equal(source.bitmap.width, 1024, 'Expected the original 1024px logo');
    assert.equal(source.bitmap.height, 1024, 'Expected the original 1024px logo');

    // Only the rounded tile and its surrounding background: the wordmark starts
    // below this crop. Include background on every side for flood-fill seeds.
    const tile = source.clone().crop({ x: 200, y: 140, w: 624, h: 632 });
    const { width, height, data } = tile.bitmap;
    const background = new Uint8Array(width * height);
    const queue = new Int32Array(width * height);
    let queued = 0;
    let processed = 0;

    function visit(x, y) {
        if (x < 0 || y < 0 || x >= width || y >= height) return;
        const pixel = y * width + x;
        const offset = pixel * 4;
        // The external white/cyan glow is light in all three channels. This is
        // NOT a global color key: enclosed cyan/white shield highlights survive.
        if (!background[pixel] && data[offset] >= 150 &&
            data[offset + 1] >= 170 && data[offset + 2] >= 175) {
            background[pixel] = 1;
            queue[queued++] = pixel;
        }
    }

    for (let x = 0; x < width; x++) {
        visit(x, 0);
        visit(x, height - 1);
    }
    for (let y = 0; y < height; y++) {
        visit(0, y);
        visit(width - 1, y);
    }
    while (processed < queued) {
        const pixel = queue[processed++];
        const x = pixel % width;
        const y = Math.floor(pixel / width);
        visit(x - 1, y);
        visit(x + 1, y);
        visit(x, y - 1);
        visit(x, y + 1);
    }

    function isBackground(x, y) {
        return x < 0 || y < 0 || x >= width || y >= height ||
            background[y * width + x] === 1;
    }
    function isEdge(x, y) {
        for (let dy = -1; dy <= 1; dy++) {
            for (let dx = -1; dx <= 1; dx++) {
                if (isBackground(x + dx, y + dy)) return true;
            }
        }
        return false;
    }

    const original = Buffer.from(data);
    let left = width;
    let top = height;
    let right = -1;
    let bottom = -1;
    for (let y = 0; y < height; y++) {
        for (let x = 0; x < width; x++) {
            const offset = (y * width + x) * 4;
            if (isBackground(x, y)) {
                data.fill(0, offset, offset + 4);
                continue;
            }
            left = Math.min(left, x);
            right = Math.max(right, x);
            top = Math.min(top, y);
            bottom = Math.max(bottom, y);
            if (!isEdge(x, y)) continue;

            // Matte only the exposed one-pixel boundary. Borrow opaque inward
            // colors to remove JPEG/white contamination, then feather coverage.
            // Interior artwork (including its bright cyan rim) stays untouched.
            const color = [0, 0, 0];
            let weight = 0;
            for (let dy = -3; dy <= 3; dy++) {
                for (let dx = -3; dx <= 3; dx++) {
                    if (isBackground(x + dx, y + dy) || isEdge(x + dx, y + dy)) continue;
                    const sample = ((y + dy) * width + x + dx) * 4;
                    const contribution = 1 / (dx * dx + dy * dy);
                    for (let channel = 0; channel < 3; channel++) {
                        color[channel] += original[sample + channel] * contribution;
                    }
                    weight += contribution;
                }
            }
            assert.ok(weight > 0, 'Unexpected isolated foreground pixel');
            let numerator = 0;
            let denominator = 0;
            for (let channel = 0; channel < 3; channel++) {
                color[channel] /= weight;
                const contrast = 255 - color[channel];
                numerator += (255 - original[offset + channel]) * contrast;
                denominator += contrast * contrast;
                data[offset + channel] = Math.round(color[channel]);
            }
            data[offset + 3] = Math.round(255 * Math.max(0.25, Math.min(1, numerator / denominator)));
        }
    }
    assert.ok(left > 0 && top > 0 && right < width - 1 && bottom < height - 1,
        'Tile must be surrounded by transparent background');
    assert.ok(right - left > 590 && bottom - top > 600, 'Unexpected tile silhouette');
    return tile.crop({ x: left, y: top, w: right - left + 1, h: bottom - top + 1 });
}

// Area-average premultiplied RGBA, not independent RGB/alpha interpolation.
// This keeps translucent edges clean against both dark and light backgrounds,
// and produces every ICO size directly from the full-resolution extracted tile.
function resizeRgba(source, size) {
    const { width, height, data } = source.bitmap;
    const output = new Jimp({ width: size, height: size, color: 0x00000000 });
    const scaleX = width / size;
    const scaleY = height / size;
    for (let y = 0; y < size; y++) {
        for (let x = 0; x < size; x++) {
            const x0 = x * scaleX;
            const y0 = y * scaleY;
            const x1 = (x + 1) * scaleX;
            const y1 = (y + 1) * scaleY;
            const color = [0, 0, 0];
            let alpha = 0;
            for (let sy = Math.floor(y0); sy < Math.ceil(y1); sy++) {
                for (let sx = Math.floor(x0); sx < Math.ceil(x1); sx++) {
                    const area = (Math.min(sx + 1, x1) - Math.max(sx, x0)) *
                        (Math.min(sy + 1, y1) - Math.max(sy, y0));
                    const offset = (sy * width + sx) * 4;
                    const coverage = data[offset + 3] / 255 * area;
                    alpha += coverage;
                    for (let channel = 0; channel < 3; channel++) {
                        color[channel] += data[offset + channel] * coverage;
                    }
                }
            }
            const offset = (y * size + x) * 4;
            output.bitmap.data[offset + 3] = Math.round(alpha / (scaleX * scaleY) * 255);
            if (output.bitmap.data[offset + 3]) {
                for (let channel = 0; channel < 3; channel++) {
                    output.bitmap.data[offset + channel] = Math.round(color[channel] / alpha);
                }
            }
        }
    }
    return output;
}

async function generate() {
    const source = await Jimp.read(path.join(webDirectory, 'assets', 'logo.png'));
    const tile = extractTile(source);
    // 20 source pixels on each side (~16px at 512): a small, symmetric margin.
    const side = Math.max(tile.bitmap.width, tile.bitmap.height) + 40;
    const canvas = new Jimp({ width: side, height: side, color: 0x00000000 });
    canvas.composite(tile, Math.floor((side - tile.bitmap.width) / 2),
        Math.floor((side - tile.bitmap.height) / 2));
    const encode = size => resizeRgba(canvas, size).getBuffer('image/png', { colorType: 6 });
    const png = await encode(512);
    // Pass explicit PNG buffers to avoid the converter's default resizing.
    const ico = await pngToIco(await Promise.all(icoSizes.map(encode)));
    assert.equal(png.subarray(0, 8).toString('hex'), '89504e470d0a1a0a');
    assert.equal(png[25], 6, 'PNG must have true RGBA channels');
    assert.equal(ico.readUInt16LE(4), icoSizes.length);
    icoSizes.forEach((size, index) => {
        assert.equal(ico[6 + index * 16] || 256, size);
        assert.equal(ico[7 + index * 16] || 256, size);
    });
    await fs.writeFile(path.join(webDirectory, 'icon.png'), png);
    await fs.writeFile(path.join(webDirectory, 'icon.ico'), ico);
    console.log('Generated RGBA icon.png (512px) and icon.ico (256/48/32/16px).');
}

generate().catch(error => {
    console.error(error);
    process.exitCode = 1;
});
