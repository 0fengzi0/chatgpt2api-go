export type ImageSizeTier = "1k" | "2k" | "4k" | "auto";

export type ImageSizePreset = {
  ratio: string;
  tier: ImageSizeTier;
  width: string;
  height: string;
  label: string;
};

function preset(
  ratio: string,
  tier: Exclude<ImageSizeTier, "auto">,
  width: number,
  height: number,
): ImageSizePreset {
  return {
    ratio,
    tier,
    width: String(width),
    height: String(height),
    label: `${ratio} · ${tier.toUpperCase()}`,
  };
}

// Keep every edge a multiple of 16, matching the Image API custom-size rules.
// 1K uses a roughly 1024px short edge; 2K uses a 1440px short edge.
export const IMAGE_SIZE_PRESETS: ImageSizePreset[] = [
  preset("1:1", "1k", 1024, 1024),
  preset("3:2", "1k", 1536, 1024),
  preset("2:3", "1k", 1024, 1536),
  preset("16:9", "1k", 1824, 1024),
  preset("9:16", "1k", 1024, 1824),
  preset("4:3", "1k", 1360, 1024),
  preset("3:4", "1k", 1024, 1360),
  preset("21:9", "1k", 2384, 1024),
  preset("9:21", "1k", 1024, 2384),
  preset("1:1", "2k", 2048, 2048),
  preset("3:2", "2k", 2160, 1440),
  preset("2:3", "2k", 1440, 2160),
  preset("16:9", "2k", 2560, 1440),
  preset("9:16", "2k", 1440, 2560),
  preset("4:3", "2k", 1920, 1440),
  preset("3:4", "2k", 1440, 1920),
  preset("21:9", "2k", 3360, 1440),
  preset("9:21", "2k", 1440, 3360),
  preset("16:9", "4k", 3840, 2160),
  preset("9:16", "4k", 2160, 3840),
  {
    ratio: "auto",
    tier: "auto",
    width: "1024",
    height: "1024",
    label: "随原图",
  },
];

export function findImageSizePreset(
  ratio: string,
  tier: string,
): ImageSizePreset | undefined {
  return IMAGE_SIZE_PRESETS.find(
    (item) => item.ratio === ratio && item.tier === tier,
  );
}

export function resolveImageSizePreset(
  ratio: string,
  tier: string,
): ImageSizePreset {
  return findImageSizePreset(ratio, tier) ?? IMAGE_SIZE_PRESETS[0];
}
