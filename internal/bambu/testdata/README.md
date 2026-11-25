These reports are written from the field names Bambu printers are known
to push (as used by Home Assistant's ha-bambulab and OrcaSlicer), not
recorded from a printer. Replace them with captures from a real P1/X1/P2S
(`mosquitto_sub -h <ip> -p 8883 --insecure -u bblp -P <code> -t 'device/+/report'`)
when one is to hand.
