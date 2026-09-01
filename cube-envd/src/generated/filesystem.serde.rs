impl serde::Serialize for CreateWatcherRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.path.is_empty() {
            len += 1;
        }
        if self.recursive {
            len += 1;
        }
        if self.include_entry {
            len += 1;
        }
        if self.allow_network_mounts {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.CreateWatcherRequest", len)?;
        if !self.path.is_empty() {
            struct_ser.serialize_field("path", &self.path)?;
        }
        if self.recursive {
            struct_ser.serialize_field("recursive", &self.recursive)?;
        }
        if self.include_entry {
            struct_ser.serialize_field("includeEntry", &self.include_entry)?;
        }
        if self.allow_network_mounts {
            struct_ser.serialize_field("allowNetworkMounts", &self.allow_network_mounts)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for CreateWatcherRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "path",
            "recursive",
            "include_entry",
            "includeEntry",
            "allow_network_mounts",
            "allowNetworkMounts",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Path,
            Recursive,
            IncludeEntry,
            AllowNetworkMounts,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "path" => Ok(GeneratedField::Path),
                            "recursive" => Ok(GeneratedField::Recursive),
                            "includeEntry" | "include_entry" => Ok(GeneratedField::IncludeEntry),
                            "allowNetworkMounts" | "allow_network_mounts" => Ok(GeneratedField::AllowNetworkMounts),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = CreateWatcherRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.CreateWatcherRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<CreateWatcherRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut path__ = None;
                let mut recursive__ = None;
                let mut include_entry__ = None;
                let mut allow_network_mounts__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Path => {
                            if path__.is_some() {
                                return Err(serde::de::Error::duplicate_field("path"));
                            }
                            path__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Recursive => {
                            if recursive__.is_some() {
                                return Err(serde::de::Error::duplicate_field("recursive"));
                            }
                            recursive__ = Some(map_.next_value()?);
                        }
                        GeneratedField::IncludeEntry => {
                            if include_entry__.is_some() {
                                return Err(serde::de::Error::duplicate_field("includeEntry"));
                            }
                            include_entry__ = Some(map_.next_value()?);
                        }
                        GeneratedField::AllowNetworkMounts => {
                            if allow_network_mounts__.is_some() {
                                return Err(serde::de::Error::duplicate_field("allowNetworkMounts"));
                            }
                            allow_network_mounts__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(CreateWatcherRequest {
                    path: path__.unwrap_or_default(),
                    recursive: recursive__.unwrap_or_default(),
                    include_entry: include_entry__.unwrap_or_default(),
                    allow_network_mounts: allow_network_mounts__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.CreateWatcherRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for CreateWatcherResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.watcher_id.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.CreateWatcherResponse", len)?;
        if !self.watcher_id.is_empty() {
            struct_ser.serialize_field("watcherId", &self.watcher_id)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for CreateWatcherResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "watcher_id",
            "watcherId",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            WatcherId,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "watcherId" | "watcher_id" => Ok(GeneratedField::WatcherId),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = CreateWatcherResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.CreateWatcherResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<CreateWatcherResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut watcher_id__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::WatcherId => {
                            if watcher_id__.is_some() {
                                return Err(serde::de::Error::duplicate_field("watcherId"));
                            }
                            watcher_id__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(CreateWatcherResponse {
                    watcher_id: watcher_id__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.CreateWatcherResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for EntryInfo {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.name.is_empty() {
            len += 1;
        }
        if self.r#type != 0 {
            len += 1;
        }
        if !self.path.is_empty() {
            len += 1;
        }
        if self.size != 0 {
            len += 1;
        }
        if self.mode != 0 {
            len += 1;
        }
        if !self.permissions.is_empty() {
            len += 1;
        }
        if !self.owner.is_empty() {
            len += 1;
        }
        if !self.group.is_empty() {
            len += 1;
        }
        if self.modified_time.is_some() {
            len += 1;
        }
        if self.symlink_target.is_some() {
            len += 1;
        }
        if !self.metadata.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.EntryInfo", len)?;
        if !self.name.is_empty() {
            struct_ser.serialize_field("name", &self.name)?;
        }
        if self.r#type != 0 {
            let v = FileType::try_from(self.r#type)
                .map_err(|_| serde::ser::Error::custom(format!("Invalid variant {}", self.r#type)))?;
            struct_ser.serialize_field("type", &v)?;
        }
        if !self.path.is_empty() {
            struct_ser.serialize_field("path", &self.path)?;
        }
        if self.size != 0 {
            #[allow(clippy::needless_borrow)]
            #[allow(clippy::needless_borrows_for_generic_args)]
            struct_ser.serialize_field("size", ToString::to_string(&self.size).as_str())?;
        }
        if self.mode != 0 {
            struct_ser.serialize_field("mode", &self.mode)?;
        }
        if !self.permissions.is_empty() {
            struct_ser.serialize_field("permissions", &self.permissions)?;
        }
        if !self.owner.is_empty() {
            struct_ser.serialize_field("owner", &self.owner)?;
        }
        if !self.group.is_empty() {
            struct_ser.serialize_field("group", &self.group)?;
        }
        if let Some(v) = self.modified_time.as_ref() {
            struct_ser.serialize_field("modifiedTime", v)?;
        }
        if let Some(v) = self.symlink_target.as_ref() {
            struct_ser.serialize_field("symlinkTarget", v)?;
        }
        if !self.metadata.is_empty() {
            struct_ser.serialize_field("metadata", &self.metadata)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for EntryInfo {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "name",
            "type",
            "path",
            "size",
            "mode",
            "permissions",
            "owner",
            "group",
            "modified_time",
            "modifiedTime",
            "symlink_target",
            "symlinkTarget",
            "metadata",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Name,
            Type,
            Path,
            Size,
            Mode,
            Permissions,
            Owner,
            Group,
            ModifiedTime,
            SymlinkTarget,
            Metadata,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "name" => Ok(GeneratedField::Name),
                            "type" => Ok(GeneratedField::Type),
                            "path" => Ok(GeneratedField::Path),
                            "size" => Ok(GeneratedField::Size),
                            "mode" => Ok(GeneratedField::Mode),
                            "permissions" => Ok(GeneratedField::Permissions),
                            "owner" => Ok(GeneratedField::Owner),
                            "group" => Ok(GeneratedField::Group),
                            "modifiedTime" | "modified_time" => Ok(GeneratedField::ModifiedTime),
                            "symlinkTarget" | "symlink_target" => Ok(GeneratedField::SymlinkTarget),
                            "metadata" => Ok(GeneratedField::Metadata),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = EntryInfo;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.EntryInfo")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<EntryInfo, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut name__ = None;
                let mut r#type__ = None;
                let mut path__ = None;
                let mut size__ = None;
                let mut mode__ = None;
                let mut permissions__ = None;
                let mut owner__ = None;
                let mut group__ = None;
                let mut modified_time__ = None;
                let mut symlink_target__ = None;
                let mut metadata__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Name => {
                            if name__.is_some() {
                                return Err(serde::de::Error::duplicate_field("name"));
                            }
                            name__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Type => {
                            if r#type__.is_some() {
                                return Err(serde::de::Error::duplicate_field("type"));
                            }
                            r#type__ = Some(map_.next_value::<FileType>()? as i32);
                        }
                        GeneratedField::Path => {
                            if path__.is_some() {
                                return Err(serde::de::Error::duplicate_field("path"));
                            }
                            path__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Size => {
                            if size__.is_some() {
                                return Err(serde::de::Error::duplicate_field("size"));
                            }
                            size__ = 
                                Some(map_.next_value::<::pbjson::private::NumberDeserialize<_>>()?.0)
                            ;
                        }
                        GeneratedField::Mode => {
                            if mode__.is_some() {
                                return Err(serde::de::Error::duplicate_field("mode"));
                            }
                            mode__ = 
                                Some(map_.next_value::<::pbjson::private::NumberDeserialize<_>>()?.0)
                            ;
                        }
                        GeneratedField::Permissions => {
                            if permissions__.is_some() {
                                return Err(serde::de::Error::duplicate_field("permissions"));
                            }
                            permissions__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Owner => {
                            if owner__.is_some() {
                                return Err(serde::de::Error::duplicate_field("owner"));
                            }
                            owner__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Group => {
                            if group__.is_some() {
                                return Err(serde::de::Error::duplicate_field("group"));
                            }
                            group__ = Some(map_.next_value()?);
                        }
                        GeneratedField::ModifiedTime => {
                            if modified_time__.is_some() {
                                return Err(serde::de::Error::duplicate_field("modifiedTime"));
                            }
                            modified_time__ = map_.next_value()?;
                        }
                        GeneratedField::SymlinkTarget => {
                            if symlink_target__.is_some() {
                                return Err(serde::de::Error::duplicate_field("symlinkTarget"));
                            }
                            symlink_target__ = map_.next_value()?;
                        }
                        GeneratedField::Metadata => {
                            if metadata__.is_some() {
                                return Err(serde::de::Error::duplicate_field("metadata"));
                            }
                            metadata__ = Some(
                                map_.next_value::<std::collections::HashMap<_, _>>()?
                            );
                        }
                    }
                }
                Ok(EntryInfo {
                    name: name__.unwrap_or_default(),
                    r#type: r#type__.unwrap_or_default(),
                    path: path__.unwrap_or_default(),
                    size: size__.unwrap_or_default(),
                    mode: mode__.unwrap_or_default(),
                    permissions: permissions__.unwrap_or_default(),
                    owner: owner__.unwrap_or_default(),
                    group: group__.unwrap_or_default(),
                    modified_time: modified_time__,
                    symlink_target: symlink_target__,
                    metadata: metadata__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.EntryInfo", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for EventType {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        let variant = match self {
            Self::Unspecified => "EVENT_TYPE_UNSPECIFIED",
            Self::Create => "EVENT_TYPE_CREATE",
            Self::Write => "EVENT_TYPE_WRITE",
            Self::Remove => "EVENT_TYPE_REMOVE",
            Self::Rename => "EVENT_TYPE_RENAME",
            Self::Chmod => "EVENT_TYPE_CHMOD",
        };
        serializer.serialize_str(variant)
    }
}
impl<'de> serde::Deserialize<'de> for EventType {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "EVENT_TYPE_UNSPECIFIED",
            "EVENT_TYPE_CREATE",
            "EVENT_TYPE_WRITE",
            "EVENT_TYPE_REMOVE",
            "EVENT_TYPE_RENAME",
            "EVENT_TYPE_CHMOD",
        ];

        struct GeneratedVisitor;

        impl serde::de::Visitor<'_> for GeneratedVisitor {
            type Value = EventType;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                write!(formatter, "expected one of: {:?}", &FIELDS)
            }

            fn visit_i64<E>(self, v: i64) -> std::result::Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                i32::try_from(v)
                    .ok()
                    .and_then(|x| x.try_into().ok())
                    .ok_or_else(|| {
                        serde::de::Error::invalid_value(serde::de::Unexpected::Signed(v), &self)
                    })
            }

            fn visit_u64<E>(self, v: u64) -> std::result::Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                i32::try_from(v)
                    .ok()
                    .and_then(|x| x.try_into().ok())
                    .ok_or_else(|| {
                        serde::de::Error::invalid_value(serde::de::Unexpected::Unsigned(v), &self)
                    })
            }

            fn visit_str<E>(self, value: &str) -> std::result::Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                match value {
                    "EVENT_TYPE_UNSPECIFIED" => Ok(EventType::Unspecified),
                    "EVENT_TYPE_CREATE" => Ok(EventType::Create),
                    "EVENT_TYPE_WRITE" => Ok(EventType::Write),
                    "EVENT_TYPE_REMOVE" => Ok(EventType::Remove),
                    "EVENT_TYPE_RENAME" => Ok(EventType::Rename),
                    "EVENT_TYPE_CHMOD" => Ok(EventType::Chmod),
                    _ => Err(serde::de::Error::unknown_variant(value, FIELDS)),
                }
            }
        }
        deserializer.deserialize_any(GeneratedVisitor)
    }
}
impl serde::Serialize for FileType {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        let variant = match self {
            Self::Unspecified => "FILE_TYPE_UNSPECIFIED",
            Self::File => "FILE_TYPE_FILE",
            Self::Directory => "FILE_TYPE_DIRECTORY",
            Self::Symlink => "FILE_TYPE_SYMLINK",
        };
        serializer.serialize_str(variant)
    }
}
impl<'de> serde::Deserialize<'de> for FileType {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "FILE_TYPE_UNSPECIFIED",
            "FILE_TYPE_FILE",
            "FILE_TYPE_DIRECTORY",
            "FILE_TYPE_SYMLINK",
        ];

        struct GeneratedVisitor;

        impl serde::de::Visitor<'_> for GeneratedVisitor {
            type Value = FileType;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                write!(formatter, "expected one of: {:?}", &FIELDS)
            }

            fn visit_i64<E>(self, v: i64) -> std::result::Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                i32::try_from(v)
                    .ok()
                    .and_then(|x| x.try_into().ok())
                    .ok_or_else(|| {
                        serde::de::Error::invalid_value(serde::de::Unexpected::Signed(v), &self)
                    })
            }

            fn visit_u64<E>(self, v: u64) -> std::result::Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                i32::try_from(v)
                    .ok()
                    .and_then(|x| x.try_into().ok())
                    .ok_or_else(|| {
                        serde::de::Error::invalid_value(serde::de::Unexpected::Unsigned(v), &self)
                    })
            }

            fn visit_str<E>(self, value: &str) -> std::result::Result<Self::Value, E>
            where
                E: serde::de::Error,
            {
                match value {
                    "FILE_TYPE_UNSPECIFIED" => Ok(FileType::Unspecified),
                    "FILE_TYPE_FILE" => Ok(FileType::File),
                    "FILE_TYPE_DIRECTORY" => Ok(FileType::Directory),
                    "FILE_TYPE_SYMLINK" => Ok(FileType::Symlink),
                    _ => Err(serde::de::Error::unknown_variant(value, FIELDS)),
                }
            }
        }
        deserializer.deserialize_any(GeneratedVisitor)
    }
}
impl serde::Serialize for FilesystemEvent {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.name.is_empty() {
            len += 1;
        }
        if self.r#type != 0 {
            len += 1;
        }
        if self.entry.is_some() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.FilesystemEvent", len)?;
        if !self.name.is_empty() {
            struct_ser.serialize_field("name", &self.name)?;
        }
        if self.r#type != 0 {
            let v = EventType::try_from(self.r#type)
                .map_err(|_| serde::ser::Error::custom(format!("Invalid variant {}", self.r#type)))?;
            struct_ser.serialize_field("type", &v)?;
        }
        if let Some(v) = self.entry.as_ref() {
            struct_ser.serialize_field("entry", v)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for FilesystemEvent {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "name",
            "type",
            "entry",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Name,
            Type,
            Entry,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "name" => Ok(GeneratedField::Name),
                            "type" => Ok(GeneratedField::Type),
                            "entry" => Ok(GeneratedField::Entry),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = FilesystemEvent;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.FilesystemEvent")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<FilesystemEvent, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut name__ = None;
                let mut r#type__ = None;
                let mut entry__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Name => {
                            if name__.is_some() {
                                return Err(serde::de::Error::duplicate_field("name"));
                            }
                            name__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Type => {
                            if r#type__.is_some() {
                                return Err(serde::de::Error::duplicate_field("type"));
                            }
                            r#type__ = Some(map_.next_value::<EventType>()? as i32);
                        }
                        GeneratedField::Entry => {
                            if entry__.is_some() {
                                return Err(serde::de::Error::duplicate_field("entry"));
                            }
                            entry__ = map_.next_value()?;
                        }
                    }
                }
                Ok(FilesystemEvent {
                    name: name__.unwrap_or_default(),
                    r#type: r#type__.unwrap_or_default(),
                    entry: entry__,
                })
            }
        }
        deserializer.deserialize_struct("filesystem.FilesystemEvent", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for GetWatcherEventsRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.watcher_id.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.GetWatcherEventsRequest", len)?;
        if !self.watcher_id.is_empty() {
            struct_ser.serialize_field("watcherId", &self.watcher_id)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for GetWatcherEventsRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "watcher_id",
            "watcherId",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            WatcherId,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "watcherId" | "watcher_id" => Ok(GeneratedField::WatcherId),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = GetWatcherEventsRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.GetWatcherEventsRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<GetWatcherEventsRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut watcher_id__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::WatcherId => {
                            if watcher_id__.is_some() {
                                return Err(serde::de::Error::duplicate_field("watcherId"));
                            }
                            watcher_id__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(GetWatcherEventsRequest {
                    watcher_id: watcher_id__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.GetWatcherEventsRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for GetWatcherEventsResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.events.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.GetWatcherEventsResponse", len)?;
        if !self.events.is_empty() {
            struct_ser.serialize_field("events", &self.events)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for GetWatcherEventsResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "events",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Events,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "events" => Ok(GeneratedField::Events),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = GetWatcherEventsResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.GetWatcherEventsResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<GetWatcherEventsResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut events__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Events => {
                            if events__.is_some() {
                                return Err(serde::de::Error::duplicate_field("events"));
                            }
                            events__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(GetWatcherEventsResponse {
                    events: events__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.GetWatcherEventsResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for ListDirRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.path.is_empty() {
            len += 1;
        }
        if self.depth != 0 {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.ListDirRequest", len)?;
        if !self.path.is_empty() {
            struct_ser.serialize_field("path", &self.path)?;
        }
        if self.depth != 0 {
            struct_ser.serialize_field("depth", &self.depth)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for ListDirRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "path",
            "depth",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Path,
            Depth,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "path" => Ok(GeneratedField::Path),
                            "depth" => Ok(GeneratedField::Depth),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = ListDirRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.ListDirRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<ListDirRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut path__ = None;
                let mut depth__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Path => {
                            if path__.is_some() {
                                return Err(serde::de::Error::duplicate_field("path"));
                            }
                            path__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Depth => {
                            if depth__.is_some() {
                                return Err(serde::de::Error::duplicate_field("depth"));
                            }
                            depth__ = 
                                Some(map_.next_value::<::pbjson::private::NumberDeserialize<_>>()?.0)
                            ;
                        }
                    }
                }
                Ok(ListDirRequest {
                    path: path__.unwrap_or_default(),
                    depth: depth__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.ListDirRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for ListDirResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.entries.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.ListDirResponse", len)?;
        if !self.entries.is_empty() {
            struct_ser.serialize_field("entries", &self.entries)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for ListDirResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "entries",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Entries,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "entries" => Ok(GeneratedField::Entries),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = ListDirResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.ListDirResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<ListDirResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut entries__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Entries => {
                            if entries__.is_some() {
                                return Err(serde::de::Error::duplicate_field("entries"));
                            }
                            entries__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(ListDirResponse {
                    entries: entries__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.ListDirResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for MakeDirRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.path.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.MakeDirRequest", len)?;
        if !self.path.is_empty() {
            struct_ser.serialize_field("path", &self.path)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for MakeDirRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "path",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Path,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "path" => Ok(GeneratedField::Path),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = MakeDirRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.MakeDirRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<MakeDirRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut path__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Path => {
                            if path__.is_some() {
                                return Err(serde::de::Error::duplicate_field("path"));
                            }
                            path__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(MakeDirRequest {
                    path: path__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.MakeDirRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for MakeDirResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if self.entry.is_some() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.MakeDirResponse", len)?;
        if let Some(v) = self.entry.as_ref() {
            struct_ser.serialize_field("entry", v)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for MakeDirResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "entry",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Entry,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "entry" => Ok(GeneratedField::Entry),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = MakeDirResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.MakeDirResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<MakeDirResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut entry__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Entry => {
                            if entry__.is_some() {
                                return Err(serde::de::Error::duplicate_field("entry"));
                            }
                            entry__ = map_.next_value()?;
                        }
                    }
                }
                Ok(MakeDirResponse {
                    entry: entry__,
                })
            }
        }
        deserializer.deserialize_struct("filesystem.MakeDirResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for MoveRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.source.is_empty() {
            len += 1;
        }
        if !self.destination.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.MoveRequest", len)?;
        if !self.source.is_empty() {
            struct_ser.serialize_field("source", &self.source)?;
        }
        if !self.destination.is_empty() {
            struct_ser.serialize_field("destination", &self.destination)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for MoveRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "source",
            "destination",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Source,
            Destination,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "source" => Ok(GeneratedField::Source),
                            "destination" => Ok(GeneratedField::Destination),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = MoveRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.MoveRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<MoveRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut source__ = None;
                let mut destination__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Source => {
                            if source__.is_some() {
                                return Err(serde::de::Error::duplicate_field("source"));
                            }
                            source__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Destination => {
                            if destination__.is_some() {
                                return Err(serde::de::Error::duplicate_field("destination"));
                            }
                            destination__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(MoveRequest {
                    source: source__.unwrap_or_default(),
                    destination: destination__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.MoveRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for MoveResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if self.entry.is_some() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.MoveResponse", len)?;
        if let Some(v) = self.entry.as_ref() {
            struct_ser.serialize_field("entry", v)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for MoveResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "entry",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Entry,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "entry" => Ok(GeneratedField::Entry),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = MoveResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.MoveResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<MoveResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut entry__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Entry => {
                            if entry__.is_some() {
                                return Err(serde::de::Error::duplicate_field("entry"));
                            }
                            entry__ = map_.next_value()?;
                        }
                    }
                }
                Ok(MoveResponse {
                    entry: entry__,
                })
            }
        }
        deserializer.deserialize_struct("filesystem.MoveResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for RemoveRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.path.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.RemoveRequest", len)?;
        if !self.path.is_empty() {
            struct_ser.serialize_field("path", &self.path)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for RemoveRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "path",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Path,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "path" => Ok(GeneratedField::Path),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = RemoveRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.RemoveRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<RemoveRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut path__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Path => {
                            if path__.is_some() {
                                return Err(serde::de::Error::duplicate_field("path"));
                            }
                            path__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(RemoveRequest {
                    path: path__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.RemoveRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for RemoveResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let len = 0;
        let struct_ser = serializer.serialize_struct("filesystem.RemoveResponse", len)?;
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for RemoveResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                            Err(serde::de::Error::unknown_field(value, FIELDS))
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = RemoveResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.RemoveResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<RemoveResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                while map_.next_key::<GeneratedField>()?.is_some() {
                    let _ = map_.next_value::<serde::de::IgnoredAny>()?;
                }
                Ok(RemoveResponse {
                })
            }
        }
        deserializer.deserialize_struct("filesystem.RemoveResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for RemoveWatcherRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.watcher_id.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.RemoveWatcherRequest", len)?;
        if !self.watcher_id.is_empty() {
            struct_ser.serialize_field("watcherId", &self.watcher_id)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for RemoveWatcherRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "watcher_id",
            "watcherId",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            WatcherId,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "watcherId" | "watcher_id" => Ok(GeneratedField::WatcherId),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = RemoveWatcherRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.RemoveWatcherRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<RemoveWatcherRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut watcher_id__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::WatcherId => {
                            if watcher_id__.is_some() {
                                return Err(serde::de::Error::duplicate_field("watcherId"));
                            }
                            watcher_id__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(RemoveWatcherRequest {
                    watcher_id: watcher_id__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.RemoveWatcherRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for RemoveWatcherResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let len = 0;
        let struct_ser = serializer.serialize_struct("filesystem.RemoveWatcherResponse", len)?;
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for RemoveWatcherResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                            Err(serde::de::Error::unknown_field(value, FIELDS))
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = RemoveWatcherResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.RemoveWatcherResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<RemoveWatcherResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                while map_.next_key::<GeneratedField>()?.is_some() {
                    let _ = map_.next_value::<serde::de::IgnoredAny>()?;
                }
                Ok(RemoveWatcherResponse {
                })
            }
        }
        deserializer.deserialize_struct("filesystem.RemoveWatcherResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for StatRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.path.is_empty() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.StatRequest", len)?;
        if !self.path.is_empty() {
            struct_ser.serialize_field("path", &self.path)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for StatRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "path",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Path,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "path" => Ok(GeneratedField::Path),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = StatRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.StatRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<StatRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut path__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Path => {
                            if path__.is_some() {
                                return Err(serde::de::Error::duplicate_field("path"));
                            }
                            path__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(StatRequest {
                    path: path__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.StatRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for StatResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if self.entry.is_some() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.StatResponse", len)?;
        if let Some(v) = self.entry.as_ref() {
            struct_ser.serialize_field("entry", v)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for StatResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "entry",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Entry,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "entry" => Ok(GeneratedField::Entry),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = StatResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.StatResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<StatResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut entry__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Entry => {
                            if entry__.is_some() {
                                return Err(serde::de::Error::duplicate_field("entry"));
                            }
                            entry__ = map_.next_value()?;
                        }
                    }
                }
                Ok(StatResponse {
                    entry: entry__,
                })
            }
        }
        deserializer.deserialize_struct("filesystem.StatResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for WatchDirRequest {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if !self.path.is_empty() {
            len += 1;
        }
        if self.recursive {
            len += 1;
        }
        if self.include_entry {
            len += 1;
        }
        if self.allow_network_mounts {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.WatchDirRequest", len)?;
        if !self.path.is_empty() {
            struct_ser.serialize_field("path", &self.path)?;
        }
        if self.recursive {
            struct_ser.serialize_field("recursive", &self.recursive)?;
        }
        if self.include_entry {
            struct_ser.serialize_field("includeEntry", &self.include_entry)?;
        }
        if self.allow_network_mounts {
            struct_ser.serialize_field("allowNetworkMounts", &self.allow_network_mounts)?;
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for WatchDirRequest {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "path",
            "recursive",
            "include_entry",
            "includeEntry",
            "allow_network_mounts",
            "allowNetworkMounts",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Path,
            Recursive,
            IncludeEntry,
            AllowNetworkMounts,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "path" => Ok(GeneratedField::Path),
                            "recursive" => Ok(GeneratedField::Recursive),
                            "includeEntry" | "include_entry" => Ok(GeneratedField::IncludeEntry),
                            "allowNetworkMounts" | "allow_network_mounts" => Ok(GeneratedField::AllowNetworkMounts),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = WatchDirRequest;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.WatchDirRequest")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<WatchDirRequest, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut path__ = None;
                let mut recursive__ = None;
                let mut include_entry__ = None;
                let mut allow_network_mounts__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Path => {
                            if path__.is_some() {
                                return Err(serde::de::Error::duplicate_field("path"));
                            }
                            path__ = Some(map_.next_value()?);
                        }
                        GeneratedField::Recursive => {
                            if recursive__.is_some() {
                                return Err(serde::de::Error::duplicate_field("recursive"));
                            }
                            recursive__ = Some(map_.next_value()?);
                        }
                        GeneratedField::IncludeEntry => {
                            if include_entry__.is_some() {
                                return Err(serde::de::Error::duplicate_field("includeEntry"));
                            }
                            include_entry__ = Some(map_.next_value()?);
                        }
                        GeneratedField::AllowNetworkMounts => {
                            if allow_network_mounts__.is_some() {
                                return Err(serde::de::Error::duplicate_field("allowNetworkMounts"));
                            }
                            allow_network_mounts__ = Some(map_.next_value()?);
                        }
                    }
                }
                Ok(WatchDirRequest {
                    path: path__.unwrap_or_default(),
                    recursive: recursive__.unwrap_or_default(),
                    include_entry: include_entry__.unwrap_or_default(),
                    allow_network_mounts: allow_network_mounts__.unwrap_or_default(),
                })
            }
        }
        deserializer.deserialize_struct("filesystem.WatchDirRequest", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for WatchDirResponse {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let mut len = 0;
        if self.event.is_some() {
            len += 1;
        }
        let mut struct_ser = serializer.serialize_struct("filesystem.WatchDirResponse", len)?;
        if let Some(v) = self.event.as_ref() {
            match v {
                watch_dir_response::Event::Start(v) => {
                    struct_ser.serialize_field("start", v)?;
                }
                watch_dir_response::Event::Filesystem(v) => {
                    struct_ser.serialize_field("filesystem", v)?;
                }
                watch_dir_response::Event::Keepalive(v) => {
                    struct_ser.serialize_field("keepalive", v)?;
                }
            }
        }
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for WatchDirResponse {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
            "start",
            "filesystem",
            "keepalive",
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
            Start,
            Filesystem,
            Keepalive,
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                        match value {
                            "start" => Ok(GeneratedField::Start),
                            "filesystem" => Ok(GeneratedField::Filesystem),
                            "keepalive" => Ok(GeneratedField::Keepalive),
                            _ => Err(serde::de::Error::unknown_field(value, FIELDS)),
                        }
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = WatchDirResponse;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.WatchDirResponse")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<WatchDirResponse, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                let mut event__ = None;
                while let Some(k) = map_.next_key()? {
                    match k {
                        GeneratedField::Start => {
                            if event__.is_some() {
                                return Err(serde::de::Error::duplicate_field("start"));
                            }
                            event__ = map_.next_value::<::std::option::Option<_>>()?.map(watch_dir_response::Event::Start)
;
                        }
                        GeneratedField::Filesystem => {
                            if event__.is_some() {
                                return Err(serde::de::Error::duplicate_field("filesystem"));
                            }
                            event__ = map_.next_value::<::std::option::Option<_>>()?.map(watch_dir_response::Event::Filesystem)
;
                        }
                        GeneratedField::Keepalive => {
                            if event__.is_some() {
                                return Err(serde::de::Error::duplicate_field("keepalive"));
                            }
                            event__ = map_.next_value::<::std::option::Option<_>>()?.map(watch_dir_response::Event::Keepalive)
;
                        }
                    }
                }
                Ok(WatchDirResponse {
                    event: event__,
                })
            }
        }
        deserializer.deserialize_struct("filesystem.WatchDirResponse", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for watch_dir_response::KeepAlive {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let len = 0;
        let struct_ser = serializer.serialize_struct("filesystem.WatchDirResponse.KeepAlive", len)?;
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for watch_dir_response::KeepAlive {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                            Err(serde::de::Error::unknown_field(value, FIELDS))
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = watch_dir_response::KeepAlive;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.WatchDirResponse.KeepAlive")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<watch_dir_response::KeepAlive, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                while map_.next_key::<GeneratedField>()?.is_some() {
                    let _ = map_.next_value::<serde::de::IgnoredAny>()?;
                }
                Ok(watch_dir_response::KeepAlive {
                })
            }
        }
        deserializer.deserialize_struct("filesystem.WatchDirResponse.KeepAlive", FIELDS, GeneratedVisitor)
    }
}
impl serde::Serialize for watch_dir_response::StartEvent {
    #[allow(deprecated)]
    fn serialize<S>(&self, serializer: S) -> std::result::Result<S::Ok, S::Error>
    where
        S: serde::Serializer,
    {
        use serde::ser::SerializeStruct;
        let len = 0;
        let struct_ser = serializer.serialize_struct("filesystem.WatchDirResponse.StartEvent", len)?;
        struct_ser.end()
    }
}
impl<'de> serde::Deserialize<'de> for watch_dir_response::StartEvent {
    #[allow(deprecated)]
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        const FIELDS: &[&str] = &[
        ];

        #[allow(clippy::enum_variant_names)]
        enum GeneratedField {
        }
        impl<'de> serde::Deserialize<'de> for GeneratedField {
            fn deserialize<D>(deserializer: D) -> std::result::Result<GeneratedField, D::Error>
            where
                D: serde::Deserializer<'de>,
            {
                struct GeneratedVisitor;

                impl serde::de::Visitor<'_> for GeneratedVisitor {
                    type Value = GeneratedField;

                    fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                        write!(formatter, "expected one of: {:?}", &FIELDS)
                    }

                    #[allow(unused_variables)]
                    fn visit_str<E>(self, value: &str) -> std::result::Result<GeneratedField, E>
                    where
                        E: serde::de::Error,
                    {
                            Err(serde::de::Error::unknown_field(value, FIELDS))
                    }
                }
                deserializer.deserialize_identifier(GeneratedVisitor)
            }
        }
        struct GeneratedVisitor;
        impl<'de> serde::de::Visitor<'de> for GeneratedVisitor {
            type Value = watch_dir_response::StartEvent;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("struct filesystem.WatchDirResponse.StartEvent")
            }

            fn visit_map<V>(self, mut map_: V) -> std::result::Result<watch_dir_response::StartEvent, V::Error>
                where
                    V: serde::de::MapAccess<'de>,
            {
                while map_.next_key::<GeneratedField>()?.is_some() {
                    let _ = map_.next_value::<serde::de::IgnoredAny>()?;
                }
                Ok(watch_dir_response::StartEvent {
                })
            }
        }
        deserializer.deserialize_struct("filesystem.WatchDirResponse.StartEvent", FIELDS, GeneratedVisitor)
    }
}
