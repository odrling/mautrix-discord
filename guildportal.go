// mautrix-discord - A Matrix-Discord puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"sync"

	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bwmarrin/discordgo"

	"go.mau.fi/mautrix-discord/database"
)

type Guild struct {
	*database.Guild

	bridge *DiscordBridge
	log    log.Logger

	roomCreateLock sync.Mutex
}

func (br *DiscordBridge) loadGuild(dbGuild *database.Guild, id string, createIfNotExist bool) *Guild {
	if dbGuild == nil {
		if id == "" || !createIfNotExist {
			return nil
		}

		dbGuild = br.DB.Guild.New()
		dbGuild.ID = id
		dbGuild.Insert()
	}

	guild := br.NewGuild(dbGuild)

	br.guildsByID[guild.ID] = guild
	if guild.MXID != "" {
		br.guildsByMXID[guild.MXID] = guild
	}

	return guild
}

func (br *DiscordBridge) GetGuildByMXID(mxid id.RoomID) *Guild {
	br.guildsLock.Lock()
	defer br.guildsLock.Unlock()

	portal, ok := br.guildsByMXID[mxid]
	if !ok {
		return br.loadGuild(br.DB.Guild.GetByMXID(mxid), "", false)
	}

	return portal
}

func (br *DiscordBridge) GetGuildByID(id string, createIfNotExist bool) *Guild {
	br.guildsLock.Lock()
	defer br.guildsLock.Unlock()

	guild, ok := br.guildsByID[id]
	if !ok {
		return br.loadGuild(br.DB.Guild.GetByID(id), id, createIfNotExist)
	}

	return guild
}

func (br *DiscordBridge) GetAllGuilds() []*Guild {
	return br.dbGuildsToGuilds(br.DB.Guild.GetAll())
}

func (br *DiscordBridge) dbGuildsToGuilds(dbGuilds []*database.Guild) []*Guild {
	br.guildsLock.Lock()
	defer br.guildsLock.Unlock()

	output := make([]*Guild, len(dbGuilds))
	for index, dbGuild := range dbGuilds {
		if dbGuild == nil {
			continue
		}

		guild, ok := br.guildsByID[dbGuild.ID]
		if !ok {
			guild = br.loadGuild(dbGuild, "", false)
		}

		output[index] = guild
	}

	return output
}

func (br *DiscordBridge) NewGuild(dbGuild *database.Guild) *Guild {
	guild := &Guild{
		Guild:  dbGuild,
		bridge: br,
		log:    br.Log.Sub(fmt.Sprintf("Guild/%s", dbGuild.ID)),
	}

	return guild
}

func (guild *Guild) getBridgeInfo() (string, event.BridgeEventContent) {
	bridgeInfo := event.BridgeEventContent{
		BridgeBot: guild.bridge.Bot.UserID,
		Creator:   guild.bridge.Bot.UserID,
		Protocol: event.BridgeInfoSection{
			ID:          "discord",
			DisplayName: "Discord",
			AvatarURL:   guild.bridge.Config.AppService.Bot.ParsedAvatar.CUString(),
			ExternalURL: "https://discord.com/",
		},
		Channel: event.BridgeInfoSection{
			ID:          guild.ID,
			DisplayName: guild.Name,
			AvatarURL:   guild.AvatarURL.CUString(),
		},
	}
	bridgeInfoStateKey := fmt.Sprintf("fi.mau.discord://discord/%s", guild.ID)
	return bridgeInfoStateKey, bridgeInfo
}

func (guild *Guild) UpdateBridgeInfo() {
	if len(guild.MXID) == 0 {
		guild.log.Debugln("Not updating bridge info: no Matrix room created")
		return
	}
	guild.log.Debugln("Updating bridge info...")
	stateKey, content := guild.getBridgeInfo()
	_, err := guild.bridge.Bot.SendStateEvent(guild.MXID, event.StateBridge, stateKey, content)
	if err != nil {
		guild.log.Warnln("Failed to update m.bridge:", err)
	}
	// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
	_, err = guild.bridge.Bot.SendStateEvent(guild.MXID, event.StateHalfShotBridge, stateKey, content)
	if err != nil {
		guild.log.Warnln("Failed to update uk.half-shot.bridge:", err)
	}
}

func (guild *Guild) CreateMatrixRoom(user *User, meta *discordgo.Guild) error {
	guild.roomCreateLock.Lock()
	defer guild.roomCreateLock.Unlock()
	if guild.MXID != "" {
		return nil
	}
	guild.log.Infoln("Creating Matrix room for guild")
	guild.UpdateInfo(user, meta)

	bridgeInfoStateKey, bridgeInfo := guild.getBridgeInfo()

	initialState := []*event.Event{{
		Type:     event.StateBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}, {
		// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
		Type:     event.StateHalfShotBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}}

	if !guild.AvatarURL.IsEmpty() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{Parsed: &event.RoomAvatarEventContent{
				URL: guild.AvatarURL,
			}},
		})
	}

	creationContent := map[string]interface{}{
		"type": event.RoomTypeSpace,
	}
	if !guild.bridge.Config.Bridge.FederateRooms {
		creationContent["m.federate"] = false
	}

	resp, err := guild.bridge.Bot.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility:      "private",
		Name:            guild.Name,
		Preset:          "private_chat",
		InitialState:    initialState,
		CreationContent: creationContent,
	})
	if err != nil {
		guild.log.Warnln("Failed to create room:", err)
		return err
	}

	guild.MXID = resp.RoomID
	guild.NameSet = true
	guild.AvatarSet = !guild.AvatarURL.IsEmpty()
	guild.Update()
	guild.bridge.guildsLock.Lock()
	guild.bridge.guildsByMXID[guild.MXID] = guild
	guild.bridge.guildsLock.Unlock()
	guild.log.Infoln("Matrix room created:", guild.MXID)

	user.ensureInvited(nil, guild.MXID, false)

	return nil
}

func (guild *Guild) UpdateInfo(source *User, meta *discordgo.Guild) *discordgo.Guild {
	if meta.Unavailable {
		return meta
	}
	changed := false
	// FIXME
	//name, err := guild.bridge.Config.Bridge.FormatChannelname(meta, user.Session)
	//if err != nil {
	//	guild.log.Warnfln("failed to format name, proceeding with generic name: %v", err)
	//	guild.Name = meta.Name
	//} else {
	//}
	changed = guild.UpdateName(meta.Name) || changed
	changed = guild.UpdateAvatar(meta.Icon) || changed
	if changed {
		guild.UpdateBridgeInfo()
		guild.Update()
	}
	return meta
}

func (guild *Guild) UpdateName(name string) bool {
	if guild.Name == name && guild.NameSet {
		return false
	}
	guild.Name = name
	guild.NameSet = false
	if guild.MXID != "" {
		_, err := guild.bridge.Bot.SetRoomName(guild.MXID, guild.Name)
		if err != nil {
			guild.log.Warnln("Failed to update room name: %s", err)
		} else {
			guild.NameSet = true
		}
	}
	return true
}

func (guild *Guild) UpdateAvatar(iconID string) bool {
	if guild.Avatar == iconID && guild.AvatarSet {
		return false
	}
	guild.AvatarSet = false
	guild.Avatar = iconID
	guild.AvatarURL = id.ContentURI{}
	if guild.Avatar != "" {
		var err error
		guild.AvatarURL, err = uploadAvatar(guild.bridge.Bot, discordgo.EndpointGuildIcon(guild.ID, iconID))
		if err != nil {
			guild.log.Warnfln("Failed to reupload guild avatar %s: %v", guild.Avatar, err)
			return true
		}
	}
	if guild.MXID != "" {
		_, err := guild.bridge.Bot.SetRoomAvatar(guild.MXID, guild.AvatarURL)
		if err != nil {
			guild.log.Warnln("Failed to update room avatar:", err)
		} else {
			guild.AvatarSet = true
		}
	}
	return true
}