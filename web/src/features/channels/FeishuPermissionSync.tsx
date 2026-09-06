import { useCallback, useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronLeft } from "lucide-react";
import QRCode from "qrcode";
import { beginFeishuRegistration, pollFeishuRegistration } from "@/lib/api-client/sdk.gen";
import type { Channel } from "@/lib/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { meQueryOptions } from "@/lib/queries/me";
import { useI18n } from "@/lib/i18n";
import { errorMessage } from "@/lib/utils";
import {
  channelString,
  normalizeChannel,
  serializePlatformConfig,
  type NormalizedChannel,
} from "./ChannelFields";

export function FeishuPermissionSync({
  channel,
  onAligned,
  overlayRoot,
}: {
  channel: NormalizedChannel;
  onAligned: (channel: NormalizedChannel) => Promise<void>;
  overlayRoot: HTMLElement | null;
}) {
  const { t } = useI18n();
  const { data: me } = useQuery(meQueryOptions);
  const [scanOpen, setScanOpen] = useState(false);
  const [qrURL, setQrURL] = useState("");
  const [status, setStatus] = useState("");
  const [scanError, setScanError] = useState("");
  const [scanning, setScanning] = useState(false);
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const stopPolling = useCallback(() => {
    if (intervalRef.current) {
      clearInterval(intervalRef.current);
      intervalRef.current = null;
    }
    setScanning(false);
  }, []);

  useEffect(() => () => stopPolling(), [stopPolling]);

  const poll = useCallback(
    async (deviceCode: string, intervalSeconds: number) => {
      try {
        const { data: result } = await pollFeishuRegistration({
          body: {
            device_code: deviceCode,
            channel_id: channel.id,
            agent_id: channelString(channel.agent_id),
            name: channel.name || "Feishu",
            is_active: channel.is_active,
            config: serializePlatformConfig("feishu", channel),
          },
          throwOnError: true,
        });
        setStatus(result.status);
        if (result.status === "slow_down") {
          stopPolling();
          const nextInterval = result.interval || intervalSeconds + 5;
          intervalRef.current = setInterval(
            () => void poll(deviceCode, nextInterval),
            nextInterval * 1000,
          );
          setScanning(true);
        }
        if (result.status === "created" && result.channel) {
          stopPolling();
          setScanOpen(false);
          // SAFETY: a successful registration response returns the Channel schema.
          await onAligned(normalizeChannel(result.channel as Channel));
        }
      } catch (error) {
        stopPolling();
        setScanError(errorMessage(error));
      }
    },
    [channel, onAligned, stopPolling],
  );

  const start = async () => {
    setScanOpen(true);
    setQrURL("");
    setStatus("waiting");
    setScanError("");
    stopPolling();
    setScanning(true);
    try {
      const { data: result } = await beginFeishuRegistration({
        body: {
          app_id: channelString(channel.app_id).trim(),
          auto_provision: Boolean(channel.auto_provision),
        },
        throwOnError: true,
      });
      setQrURL(await QRCode.toDataURL(result.qr_url, { width: 256, margin: 2 }));
      const intervalSeconds = result.interval || 5;
      intervalRef.current = setInterval(
        () => void poll(result.device_code, intervalSeconds),
        intervalSeconds * 1000,
      );
      setScanning(true);
    } catch (error) {
      setScanError(errorMessage(error));
      setScanning(false);
    }
  };

  if (channel.type !== "feishu" || !me?.is_admin) return null;

  const canStart = Boolean(
    channelString(channel.app_id).trim() && channelString(channel.agent_id).trim(),
  );

  return (
    <>
      <div className="space-y-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          loading={scanning}
          disabled={!canStart || scanning}
          onClick={() => void start()}
        >
          {t("channels.scanAlignFeishu")}
        </Button>
        <p className="text-xs text-muted-foreground">
          {canStart ? t("channels.scanAlignFeishuDesc") : t("channels.scanAlignFeishuUnavailable")}
        </p>
      </div>

      {scanOpen &&
        overlayRoot &&
        createPortal(
          <div className="absolute inset-0 z-10 flex flex-col bg-background">
            <div className="flex shrink-0 items-center gap-2 border-b p-4">
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                aria-label={t("common.back")}
                onClick={() => {
                  stopPolling();
                  setScanOpen(false);
                }}
              >
                <ChevronLeft size={16} />
              </Button>
              <div className="min-w-0 flex-1">
                <p className="text-sm font-semibold">{t("channels.scanAlignFeishuTitle")}</p>
                <p className="text-xs text-muted-foreground">
                  {t("channels.scanAlignFeishuScanDesc")}
                </p>
              </div>
            </div>
            <div className="min-h-0 flex-1 overflow-y-auto p-5">
              <div className="flex flex-col items-center gap-4 text-center">
                {qrURL && (
                  <img
                    src={qrURL}
                    alt={t("channels.scanAlignFeishuQrAlt")}
                    className="size-48 max-w-full"
                  />
                )}
                {!qrURL && !scanError && <Spinner className="size-4" />}
                <Badge
                  size="sm"
                  variant={scanError ? "error" : status === "created" ? "success" : "warning"}
                  className="max-w-full whitespace-normal"
                >
                  {scanError || status || t("channels.waiting")}
                </Badge>
              </div>
            </div>
            <div className="flex shrink-0 items-center justify-end gap-2 border-t p-4">
              <Button
                type="button"
                variant="ghost"
                onClick={() => {
                  stopPolling();
                  setScanOpen(false);
                }}
              >
                {t("common.cancel")}
              </Button>
            </div>
          </div>,
          overlayRoot,
        )}
    </>
  );
}
